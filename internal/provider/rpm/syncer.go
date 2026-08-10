package rpm

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
	// syncLeaseDuration is how long a claimed sync lease is held before it is
	// considered abandoned. It comfortably exceeds a scan's own timeout so a live
	// scan never loses its lease, while a crashed replica's lease still expires.
	syncLeaseDuration = 15 * time.Minute
	// defaultSyncFreshness is the periodic re-check interval used when a remote's
	// mutable_ttl is unset.
	defaultSyncFreshness = 5 * time.Minute
	// jobQueueDepth bounds the pending work queue; enqueues past it are dropped
	// (a later poll re-enqueues), never blocking the caller.
	jobQueueDepth = 256
)

// SyncStore is the persistence surface the syncer needs: the metadata cache it
// primes plus the shared sync-state coordination (remote enumeration and the
// per-remote lease). *database.DB satisfies it.
type SyncStore interface {
	provider.RemoteMetadataStore
	ListGitHubRPMRemotes(ctx context.Context) ([]models.Remote, error)
	ClaimGitHubSyncLease(ctx context.Context, remoteName, owner string, freshness, lease time.Duration) (claimed bool, etag string, err error)
	ReleaseGitHubSyncLease(ctx context.Context, remoteName, owner, etag string, syncedAt time.Time) error
}

// SyncConfig tunes the shared syncer. Zero values fall back to safe defaults.
type SyncConfig struct {
	RatePerSec   float64       // global GitHub request rate (req/s)
	Burst        int           // token-bucket burst
	Workers      int           // concurrent scan workers
	PollInterval time.Duration // base scheduler tick; per-remote cadence is mutable_ttl
}

type syncJob struct {
	remote models.Remote
	prime  bool
}

// Syncer is the single per-process background worker that keeps every
// github_rpm remote's derived metadata fresh. It owns a deduped work queue, a
// pool of workers, and a global token-bucket rate limiter shared across all
// remotes and bound onto the github provider so every GitHub call it makes
// passes through the same bucket. Periodic checks are gated by a shared DB lease
// so, across replicas, only one performs each scan.
type Syncer struct {
	store   SyncStore
	prov    *GitHubProvider
	limiter *rate.Limiter
	cfg     SyncConfig
	owner   string

	jobs   chan syncJob
	mu     sync.Mutex
	active map[string]bool // remotes queued or in-flight, for dedup/coalescing
}

// NewSyncer builds the syncer bound to the process-wide github provider
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
	// Bind the shared limiter and back-reference so the request path routes
	// through this syncer and every derive HTTP call is rate limited.
	prov.limiter = lim
	prov.syncer = s
	return s
}

// Run starts the worker pool and the periodic scheduler and blocks until ctx is
// canceled, at which point it drains in-flight scans and returns.
func (s *Syncer) Run(ctx context.Context) {
	slog.Info("github_rpm syncer started",
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

	s.schedule(ctx) // sweep at boot so existing remotes are checked immediately
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			slog.Info("github_rpm syncer stopped")
			return
		case <-ticker.C:
			s.schedule(ctx)
		}
	}
}

// schedule enqueues a periodic check for every github_rpm remote. The DB lease
// (claimed in the worker) enforces the per-remote mutable_ttl cadence and cross
// replica coordination, so enqueuing every tick is cheap: a not-yet-due remote
// simply fails to claim and is skipped.
func (s *Syncer) schedule(ctx context.Context) {
	remotes, err := s.store.ListGitHubRPMRemotes(ctx)
	if err != nil {
		slog.Error("github_rpm syncer: list remotes", "error", err)
		return
	}
	for _, r := range remotes {
		s.enqueue(r, false)
	}
}

// EnqueuePrime queues an immediate background prime for a freshly created
// remote so its metadata is derived without blocking the create call.
func (s *Syncer) EnqueuePrime(remote models.Remote) {
	if s == nil {
		return
	}
	s.enqueue(remote, true)
}

// enqueue adds a job unless the remote is already queued or in-flight, coalescing
// duplicate requests down to one scan. It never blocks: a full queue drops the
// job (a later poll re-enqueues it) after clearing the dedup slot.
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

// process claims the shared lease and, if won, runs an incremental scan. The
// lease bounds total GitHub load to one scan per freshness window across all
// replicas; losing the claim (another replica scanning, or not yet due) is a
// no-op.
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
		freshness = 0 // prime ignores the recency gate but still respects a live lease
	}

	claimed, etag, err := s.store.ClaimGitHubSyncLease(ctx, job.remote.Name, s.owner, freshness, syncLeaseDuration)
	if err != nil {
		slog.Error("github_rpm syncer: claim lease", "remote", job.remote.Name, "error", err)
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
		slog.Error("github_rpm syncer: scan failed", "remote", job.remote.Name, "error", scanErr)
	}

	// Release on a detached context so a clean shutdown mid-scan still frees the
	// lease and advances last_synced_at (otherwise it simply expires).
	relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer relCancel()
	if err := s.store.ReleaseGitHubSyncLease(relCtx, job.remote.Name, s.owner, releaseEtag, time.Now()); err != nil {
		slog.Warn("github_rpm syncer: release lease", "remote", job.remote.Name, "error", err)
	}

	if scanErr == nil && changed {
		slog.Info("github_rpm syncer: refreshed", "remote", job.remote.Name, "prime", job.prime)
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
