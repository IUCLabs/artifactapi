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

	v1 "github.com/IUCLabs/artifactapi/internal/api/v1"
	v2 "github.com/IUCLabs/artifactapi/internal/api/v2"
	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/config"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/gc"
	_ "github.com/IUCLabs/artifactapi/internal/provider/alpine"
	_ "github.com/IUCLabs/artifactapi/internal/provider/docker"
	_ "github.com/IUCLabs/artifactapi/internal/provider/generic"
	_ "github.com/IUCLabs/artifactapi/internal/provider/goproxy"
	_ "github.com/IUCLabs/artifactapi/internal/provider/helm"
	_ "github.com/IUCLabs/artifactapi/internal/provider/npm"
	_ "github.com/IUCLabs/artifactapi/internal/provider/puppet"
	_ "github.com/IUCLabs/artifactapi/internal/provider/pypi"
	_ "github.com/IUCLabs/artifactapi/internal/provider/rpm"
	_ "github.com/IUCLabs/artifactapi/internal/provider/terraform"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/virtual"
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
	gc           *gc.Collector
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

	engine := proxy.NewEngine(db, redis, s3)
	localHandler := v2.NewLocalHandler(db, s3)
	virtEngine := virtual.NewEngine(db, engine)
	collector := gc.New(db, s3, 1*time.Hour)

	s := &Server{
		cfg:          cfg,
		version:      version,
		db:           db,
		cache:        redis,
		store:        s3,
		engine:       engine,
		virtEngine:   virtEngine,
		localHandler: localHandler,
		gc:           collector,
	}

	s.router = s.routes()
	return s, nil
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(NewStructuredLogger())
	r.Use(middleware.Recoverer)

	r.Use(cors)

	r.Get("/health", s.handleHealth)
	r.Get("/", s.handleRoot)

	proxyHandler := v1.NewProxyHandler(s.engine, s.virtEngine, s.db, s.store, s.localHandler)
	r.Mount("/api/v1", proxyHandler.Routes())
	r.Mount("/v2", proxyHandler.DockerV2Routes())

	remotesHandler := v2.NewRemotesHandler(s.db)
	virtualsHandler := v2.NewVirtualsHandler(s.db)
	healthHandler := v2.NewHealthHandler(s.db, s.cache, s.store)
	statsHandler := v2.NewStatsHandler(s.db)
	eventsHandler := v2.NewEventsHandler()
	probeHandler := v2.NewProbeHandler(s.engine, s.db)

	r.Route("/api/v2", func(r chi.Router) {
		r.Mount("/remotes", remotesHandler.Routes())
		r.Mount("/virtuals", virtualsHandler.Routes())
		r.Mount("/health", healthHandler.Routes())
		r.Mount("/stats", statsHandler.Routes())
		r.Mount("/events", eventsHandler.Routes())
		r.Mount("/probe", probeHandler.Routes())

		r.Route("/remotes/{name}/objects", func(r chi.Router) {
			objHandler := v2.NewObjectsHandler(s.db)
			r.Get("/", objHandler.Routes().ServeHTTP)
			r.Delete("/*", objHandler.Routes().ServeHTTP)
		})

		r.Route("/locals/{name}/objects", func(r chi.Router) {
			objHandler := v2.NewObjectsHandler(s.db)
			r.Get("/", objHandler.LocalRoutes().ServeHTTP)
			r.Delete("/*", objHandler.LocalRoutes().ServeHTTP)
		})

		r.Route("/remotes/{name}/files", func(r chi.Router) {
			r.Put("/*", s.localHandler.Routes().ServeHTTP)
			r.Get("/*", s.localHandler.Routes().ServeHTTP)
			r.Delete("/*", s.localHandler.Routes().ServeHTTP)
		})
	})

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"name":"artifactapi","version":"%s"}`, s.version)
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
