// Package testsupport starts throwaway backing containers (Postgres, Redis,
// MinIO) for integration-style unit tests. It is only ever imported from
// *_test.go files, so it never reaches the production binary. Each Start*
// function returns a connection detail plus a terminate func; callers wire
// them up in a TestMain and skip the package's tests when Docker is absent.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

func init() {
	// The Ryuk reaper container cannot start in this environment; each Start*
	// returns an explicit terminate func for cleanup instead.
	if _, ok := os.LookupEnv("TESTCONTAINERS_RYUK_DISABLED"); !ok {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// StartPostgres launches postgres:17-alpine and returns its DSN.
func StartPostgres(ctx context.Context) (dsn string, terminate func(), err error) {
	c, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("artifacts"),
		tcpostgres.WithUsername("artifacts"),
		tcpostgres.WithPassword("artifacts123"),
		testcontainers.WithWaitStrategy(
			// Postgres opens the port, runs init scripts, then restarts, so wait
			// for the readiness log to appear twice to avoid connection resets.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", nil, err
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5432/tcp")
	dsn = fmt.Sprintf("postgres://artifacts:artifacts123@%s:%s/artifacts?sslmode=disable", host, port.Port())
	return dsn, func() { _ = c.Terminate(ctx) }, nil
}

// StartRedis launches redis:7-alpine and returns its URL.
func StartRedis(ctx context.Context) (url string, terminate func(), err error) {
	c, err := tcredis.Run(ctx,
		"redis:7-alpine",
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("6379/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", nil, err
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "6379/tcp")
	url = fmt.Sprintf("redis://%s:%s", host, port.Port())
	return url, func() { _ = c.Terminate(ctx) }, nil
}

// MinioConn holds MinIO connection details.
type MinioConn struct {
	Endpoint  string
	AccessKey string
	SecretKey string
}

// StartMinio launches minio and returns its connection details.
func StartMinio(ctx context.Context) (conn MinioConn, terminate func(), err error) {
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372",
			ExposedPorts: []string{"9000/tcp"},
			Cmd:          []string{"server", "/data"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     "minioadmin",
				"MINIO_ROOT_PASSWORD": "minioadmin",
			},
			WaitingFor: wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return MinioConn{}, nil, err
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "9000/tcp")
	return MinioConn{
		Endpoint:  fmt.Sprintf("%s:%s", host, port.Port()),
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
	}, func() { _ = c.Terminate(ctx) }, nil
}
