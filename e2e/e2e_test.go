//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/IUCLabs/artifactapi/internal/config"
	"github.com/IUCLabs/artifactapi/internal/server"
)

var baseURL string

func TestMain(m *testing.M) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("artifacts"),
		tcpostgres.WithUsername("artifacts"),
		tcpostgres.WithPassword("artifacts123"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pgContainer.Terminate(ctx)

	redisContainer, err := tcredis.Run(ctx,
		"redis:7-alpine",
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("6379/tcp").WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer redisContainer.Terminate(ctx)

	minioContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "pgsty/minio:RELEASE.2026-08-04T00-00-00Z",
			ExposedPorts: []string{"9000/tcp"},
			Cmd:          []string{"server", "/data"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     "minioadmin",
				"MINIO_ROOT_PASSWORD": "minioadmin",
			},
			WaitingFor: wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		log.Fatalf("minio: %v", err)
	}
	defer minioContainer.Terminate(ctx)

	pgHost, _ := pgContainer.Host(ctx)
	pgPort, _ := pgContainer.MappedPort(ctx, "5432/tcp")
	redisHost, _ := redisContainer.Host(ctx)
	redisPort, _ := redisContainer.MappedPort(ctx, "6379/tcp")
	minioHost, _ := minioContainer.Host(ctx)
	minioPort, _ := minioContainer.MappedPort(ctx, "9000/tcp")

	os.Setenv("DBHOST", pgHost)
	os.Setenv("DBPORT", pgPort.Port())
	os.Setenv("DBUSER", "artifacts")
	os.Setenv("DBPASS", "artifacts123")
	os.Setenv("DBNAME", "artifacts")
	os.Setenv("DBSSL", "disable")
	os.Setenv("REDIS_URL", fmt.Sprintf("redis://%s:%s", redisHost, redisPort.Port()))
	os.Setenv("MINIO_ENDPOINT", fmt.Sprintf("%s:%s", minioHost, minioPort.Port()))
	os.Setenv("MINIO_ACCESS_KEY", "minioadmin")
	os.Setenv("MINIO_SECRET_KEY", "minioadmin")
	os.Setenv("MINIO_BUCKET", "artifacts-test")
	os.Setenv("MINIO_SECURE", "false")
	os.Setenv("LISTEN_ADDR", ":0")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.ListenAddr = "127.0.0.1:0"

	srv, err := server.New(cfg, "e2e-test")
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	srvCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	addr := startServer(srvCtx, srv)
	baseURL = "http://" + addr

	code := m.Run()
	cancel()
	os.Exit(code)
}

func startServer(ctx context.Context, srv *server.Server) string {
	ln, err := findListener()
	if err != nil {
		log.Fatalf("listener: %v", err)
	}
	addr := ln.Addr().String()

	go srv.RunOnListener(ctx, ln)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/health")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return addr
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("server did not start in time at %s", addr)
	return ""
}
