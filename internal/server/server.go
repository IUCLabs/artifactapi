package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	tfregistry "github.com/IUCLabs/artifactapi/internal/api/terraform"
	v1 "github.com/IUCLabs/artifactapi/internal/api/v1"
	v2 "github.com/IUCLabs/artifactapi/internal/api/v2"
	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/config"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/gc"
	"github.com/IUCLabs/artifactapi/internal/githubauth"
	_ "github.com/IUCLabs/artifactapi/internal/provider/all"
	"github.com/IUCLabs/artifactapi/internal/provider/alpine"
	"github.com/IUCLabs/artifactapi/internal/provider/deb"
	"github.com/IUCLabs/artifactapi/internal/provider/rpm"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/tfsign"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type Server struct {
	cfg          *config.Config
	version      string
	router       chi.Router
	db           *database.DB
	cache        *cache.Redis
	store        *storage.S3
	engine       *proxy.Engine
	virtEngine   *virtual.Engine
	localHandler *v2.LocalHandler
	tfRegistry   *tfregistry.Handler
	gc           *gc.Collector
	syncer       *rpm.Syncer
	debSyncer    *deb.Syncer
	alpineSyncer *alpine.Syncer
}

func New(cfg *config.Config, version string) (*Server, error) {
	db, err := database.New(cfg.DatabaseDSN())
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	redis, err := cache.NewRedis(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}

	s3, err := storage.NewS3(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3Secure, cfg.S3Region)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}

	// Install the process-wide GitHub credential before any provider makes an
	// outbound call. A misconfiguration (e.g. App id without a private key) fails
	// closed here rather than silently falling back to anonymous. No credential
	// configured is fine — requests stay anonymous.
	ghCred, err := githubauth.New(githubauth.Options{
		Token:          cfg.GitHubToken,
		AppID:          cfg.GitHubAppID,
		InstallationID: cfg.GitHubAppInstallationID,
		PrivateKeyPEM:  cfg.GitHubAppPrivateKey,
		PrivateKeyPath: cfg.GitHubAppPrivateKeyPath,
	})
	if err != nil {
		return nil, fmt.Errorf("github auth: %w", err)
	}
	githubauth.SetServer(ghCred)
	if ghCred != nil {
		slog.Info("github machine credential configured")
	}

	engine := proxy.NewEngine(db, redis, s3)
	localHandler := v2.NewLocalHandler(db, s3)
	virtEngine := virtual.NewEngine(db, engine)
	collector := gc.New(db, s3, 1*time.Hour)
	syncer := rpm.NewSyncer(db, rpm.SyncConfig{
		RatePerSec:   cfg.GitHubSyncRatePerSec,
		Burst:        cfg.GitHubSyncBurst,
		Workers:      cfg.GitHubSyncWorkers,
		PollInterval: time.Duration(cfg.GitHubSyncPollInterval) * time.Second,
	})
	debSyncer := deb.NewSyncer(db, deb.SyncConfig{
		RatePerSec:   cfg.GitHubSyncRatePerSec,
		Burst:        cfg.GitHubSyncBurst,
		Workers:      cfg.GitHubSyncWorkers,
		PollInterval: time.Duration(cfg.GitHubSyncPollInterval) * time.Second,
	})
	alpineSyncer := alpine.NewSyncer(db, alpine.SyncConfig{
		RatePerSec:   cfg.GitHubSyncRatePerSec,
		Burst:        cfg.GitHubSyncBurst,
		Workers:      cfg.GitHubSyncWorkers,
		PollInterval: time.Duration(cfg.GitHubSyncPollInterval) * time.Second,
	})

	// The terraform registry signs with a GPG key. A configured file wins (BYO
	// key); otherwise artifactapi generates one on first start and persists it in
	// the database so every replica shares it. A failure here must not take the
	// server down — the registry just stays disabled.
	var signer *tfsign.Signer
	if cfg.TFSigningKeyPath != "" {
		signer, err = tfsign.Load(cfg.TFSigningKeyPath, cfg.TFSigningKeyPassphrase)
	} else {
		signer, err = tfsign.LoadOrCreate(context.Background(), db, "terraform-provider")
	}
	if err != nil {
		slog.Warn("terraform provider registry disabled", "error", err)
		signer = nil
	}
	tfRegistry := tfregistry.NewHandler(db, signer, cfg.TFProviderProtocols)
	if tfRegistry.Enabled() {
		slog.Info("terraform provider registry enabled", "key_id", signer.KeyID())
	}

	s := &Server{
		cfg:          cfg,
		version:      version,
		db:           db,
		cache:        redis,
		store:        s3,
		engine:       engine,
		virtEngine:   virtEngine,
		localHandler: localHandler,
		tfRegistry:   tfRegistry,
		gc:           collector,
		syncer:       syncer,
		debSyncer:    debSyncer,
		alpineSyncer: alpineSyncer,
	}

	s.router = s.routes()
	return s, nil
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// The ingress appends the peer it saw to X-Forwarded-For; entries left of it are client-supplied.
	r.Use(middleware.ClientIPFromXFFTrustedProxies(1))
	r.Use(NewStructuredLogger())
	r.Use(middleware.Recoverer)

	r.Use(cors)

	r.Get("/health", s.handleHealth)
	r.Get("/", s.handleRoot)
	r.Get("/version", s.handleVersion)

	// Terraform provider registry: service discovery at the well-known path,
	// providers.v1 protocol under /terraform/v1/providers.
	r.Get("/.well-known/terraform.json", s.tfRegistry.ServiceDiscovery)
	r.Mount(tfregistry.MountPath, s.tfRegistry.Routes())

	proxyHandler := v1.NewProxyHandler(s.engine, s.virtEngine, s.db, s.store, s.localHandler)
	s.virtEngine.NPMLocal = proxyHandler.LocalNPMPackument
	s.virtEngine.GemsLocal = proxyHandler.LocalGemsIndex
	s.virtEngine.AnsibleLocal = proxyHandler.LocalAnsible
	writeAuth := auth.NewBearerOIDC(s.cfg.OIDCIssuer, s.cfg.OIDCAudiences)
	uploadAuth := passthrough
	dockerV2 := http.Handler(proxyHandler.DockerV2Routes())
	if s.cfg.AuthUploads {
		uploadAuth = writeAuth.RequireWrites
		dockerV2 = writeAuth.RequireRegistryWrites(dockerV2, func(r *http.Request) string {
			return v1.Scheme(r) + "://" + r.Host + "/v2/token"
		})
	}
	r.Mount("/api/v1", uploadAuth(proxyHandler.Routes()))
	r.Mount("/v2", dockerV2)

	remotesHandler := v2.NewRemotesHandler(s.db, s.cache, map[models.PackageType]v2.Primer{
		models.PackageGitHubRPM:    s.syncer,
		models.PackageGitHubDeb:    s.debSyncer,
		models.PackageGitHubAlpine: s.alpineSyncer,
	})
	virtualsHandler := v2.NewVirtualsHandler(s.db)
	healthHandler := v2.NewHealthHandler(s.db, s.cache, s.store)
	statsHandler := v2.NewStatsHandler(s.db)
	eventsHandler := v2.NewEventsHandler()
	probeHandler := v2.NewProbeHandler(s.engine, s.db)
	settingsHandler := v2.NewSettingsHandler(s.db, s.probeConfig())

	r.Route("/api/v2", func(r chi.Router) {
		r.Mount("/remotes", writeAuth.RequireWrites(remotesHandler.Routes()))
		r.Mount("/virtuals", writeAuth.RequireWrites(virtualsHandler.Routes()))
		r.Mount("/settings", writeAuth.RequireWrites(settingsHandler.Routes()))
		r.Mount("/health", healthHandler.Routes())
		r.Mount("/stats", statsHandler.Routes())
		r.Mount("/events", eventsHandler.Routes())
		r.Mount("/probe", probeHandler.Routes())

		r.With(uploadAuth).Route("/remotes/{name}/objects", func(r chi.Router) {
			objHandler := v2.NewObjectsHandler(s.db)
			r.Get("/", objHandler.Routes(s.engine).ServeHTTP)
			r.Delete("/*", objHandler.Routes(s.engine).ServeHTTP)
		})

		r.With(uploadAuth).Route("/locals/{name}/objects", func(r chi.Router) {
			objHandler := v2.NewObjectsHandler(s.db)
			r.Get("/", objHandler.LocalRoutes().ServeHTTP)
			r.Delete("/*", objHandler.LocalRoutes().ServeHTTP)
		})

		r.With(uploadAuth).Route("/remotes/{name}/files", func(r chi.Router) {
			r.Put("/*", s.localHandler.Routes().ServeHTTP)
			r.Get("/*", s.localHandler.Routes().ServeHTTP)
			r.Delete("/*", s.localHandler.Routes().ServeHTTP)
		})
	})

	return r
}

func passthrough(h http.Handler) http.Handler { return h }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"status":"ok"}`)
}

// handleRoot sends browsers landing on the bare domain to the web UI, which is
// served under /ui. The service identity that used to live here is at /version.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"name":"artifactapi","version":"%s"}`, s.version)
}

func (s *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

func (s *Server) Run(ctx context.Context) error {
	go s.gc.Run(ctx)
	go s.syncer.Run(ctx)
	go s.debSyncer.Run(ctx)
	go s.alpineSyncer.Run(ctx)
	go proxy.RunStoreProbe(ctx, s.probeConfig(), s.db.GetSettings, s.store.Probe, s.cache)

	httpServer := s.newHTTPServer()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("starting server", "addr", s.cfg.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) RunOnListener(ctx context.Context, ln net.Listener) error {
	go s.gc.Run(ctx)
	go s.syncer.Run(ctx)
	go s.debSyncer.Run(ctx)
	go s.alpineSyncer.Run(ctx)
	go proxy.RunStoreProbe(ctx, s.probeConfig(), s.db.GetSettings, s.store.Probe, s.cache)

	httpServer := s.newHTTPServer()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("starting server", "addr", ln.Addr().String())
	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) probeConfig() proxy.ProbeConfig {
	return proxy.ProbeConfig{
		UpThreshold:   s.cfg.S3HealthUpThreshold,
		DownThreshold: s.cfg.S3HealthDownThreshold,
		Interval:      s.cfg.S3HealthInterval,
		Timeout:       s.cfg.S3HealthTimeout,
	}
}
