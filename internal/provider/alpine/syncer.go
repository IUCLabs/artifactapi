package alpine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	syncLeaseDuration    = 15 * time.Minute
	defaultSyncFreshness = 5 * time.Minute
	jobQueueDepth        = 256
)

// SyncStore is the persistence surface the alpine syncer needs: the metadata
// cache it primes plus the shared sync-state coordination (remote enumeration
// and the per-remote lease). *database.DB satisfies it.
type SyncStore interface {
	provider.RemoteMetadataStore
	ListGitHubAlpineRemotes(ctx context.Context) ([]models.Remote, error)
	ClaimGitHubAlpineSyncLease(ctx context.Context, remoteName, owner string, freshness, lease time.Duration) (claimed bool, etag string, err error)
	ReleaseGitHubAlpineSyncLease(ctx context.Context, remoteName, owner, etag string, syncedAt time.Time) error
}

// SyncConfig tunes the shared syncer. Zero values fall back to safe defaults.
type SyncConfig struct {
	RatePerSec   float64
	Burst        int
	Workers      int
	PollInterval time.Duration
}

type syncJob struct {
	remote models.Remote
	prime  bool
}

// Syncer is the single per-process background worker that keeps every
// github_alpine remote's derived metadata fresh. It owns a deduped work queue, a
// pool of workers, and a global token-bucket rate limiter shared across all
// remotes and bound onto the github_alpine provider. Periodic checks are gated by
// a shared DB lease so, across replicas, only one performs each scan.
type Syncer struct {
	store   SyncStore
	prov    *GitHubProvider
	limiter *rate.Limiter
	cfg     SyncConfig
	owner   string

	jobs   chan syncJob
	mu     sync.Mutex
	active map[string]bool
}

// NewSyncer builds the syncer bound to the process-wide github_alpine provider
// singleton. Call Run to start it.
func NewSyncer(store SyncStore, cfg SyncConfig) *Syncer {
	return newSyncer(store, gitHubProvider, cfg)
}

func newSyncer(store SyncStore, prov *GitHubProvider, cfg SyncConfig) *Syncer {
	if cfg.RatePerSec <= 0 {
		cfg.RatePerSec = 1
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 5
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 3
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}

	lim := rate.NewLimiter(rate.Limit(cfg.RatePerSec), cfg.Burst)
	s := &Syncer{
		store:   store,
		prov:    prov,
		limiter: lim,
		cfg:     cfg,
		owner:   leaseOwner(),
		jobs:    make(chan syncJob, jobQueueDepth),
		active:  map[string]bool{},
	}
	prov.limiter = lim
	prov.syncer = s
	return s
}

// Run starts the worker pool and the periodic scheduler and blocks until ctx is
// canceled, at which point it drains in-flight scans and returns.
func (s *Syncer) Run(ctx context.Context) {
	slog.Info("github_alpine syncer started",
		"rate_per_sec", s.cfg.RatePerSec, "burst", s.cfg.Burst,
		"workers", s.cfg.Workers, "poll_interval", s.cfg.PollInterval, "owner", s.owner)

	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx)
		}()
	}

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	s.schedule(ctx)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			slog.Info("github_alpine syncer stopped")
			return
		case <-ticker.C:
			s.schedule(ctx)
		}
	}
}

// schedule enqueues a periodic check for every github_alpine remote. The DB lease
// enforces the per-remote mutable_ttl cadence and cross-replica coordination.
func (s *Syncer) schedule(ctx context.Context) {
	remotes, err := s.store.ListGitHubAlpineRemotes(ctx)
	if err != nil {
		slog.Error("github_alpine syncer: list remotes", "error", err)
		return
	}
	for _, r := range remotes {
		s.enqueue(r, false)
	}
}

// EnqueuePrime queues an immediate background prime for a freshly created remote.
func (s *Syncer) EnqueuePrime(remote models.Remote) {
	if s == nil {
		return
	}
	s.enqueue(remote, true)
}

// enqueue adds a job unless the remote is already queued or in-flight, coalescing
// duplicate requests down to one scan. It never blocks.
func (s *Syncer) enqueue(remote models.Remote, prime bool) {
	s.mu.Lock()
	if s.active[remote.Name] {
		s.mu.Unlock()
		return
	}
	s.active[remote.Name] = true
	s.mu.Unlock()

	select {
	case s.jobs <- syncJob{remote: remote, prime: prime}:
	default:
		s.mu.Lock()
		delete(s.active, remote.Name)
		s.mu.Unlock()
	}
}

func (s *Syncer) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			s.process(ctx, job)
		}
	}
}

// process claims the shared lease and, if won, runs an incremental scan. Losing
// the claim (another replica scanning, or not yet due) is a no-op.
func (s *Syncer) process(ctx context.Context, job syncJob) {
	defer func() {
		s.mu.Lock()
		delete(s.active, job.remote.Name)
		s.mu.Unlock()
	}()

	freshness := time.Duration(job.remote.MutableTTL) * time.Second
	if freshness <= 0 {
		freshness = defaultSyncFreshness
	}
	if job.prime {
		freshness = 0
	}

	claimed, etag, err := s.store.ClaimGitHubAlpineSyncLease(ctx, job.remote.Name, s.owner, freshness, syncLeaseDuration)
	if err != nil {
		slog.Error("github_alpine syncer: claim lease", "remote", job.remote.Name, "error", err)
		return
	}
	if !claimed {
		return
	}

	scanCtx, cancel := context.WithTimeout(ctx, s.prov.scanTimeout)
	defer cancel()

	newEtag, changed, scanErr := s.prov.scanWithState(scanCtx, job.remote, s.store, etag)
	releaseEtag := etag
	if scanErr == nil {
		releaseEtag = newEtag
	} else {
		slog.Error("github_alpine syncer: scan failed", "remote", job.remote.Name, "error", scanErr)
	}

	relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer relCancel()
	if err := s.store.ReleaseGitHubAlpineSyncLease(relCtx, job.remote.Name, s.owner, releaseEtag, time.Now()); err != nil {
		slog.Warn("github_alpine syncer: release lease", "remote", job.remote.Name, "error", err)
	}

	if scanErr == nil && changed {
		slog.Info("github_alpine syncer: refreshed", "remote", job.remote.Name, "prime", job.prime)
	}
}

// leaseOwner is a per-replica identity for the lease: hostname plus a random
// suffix so restarts and colocated replicas never collide.
func leaseOwner() string {
	host, _ := os.Hostname()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}
