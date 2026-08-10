package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	ListenAddr string

	DBHost string
	DBPort int
	DBUser string
	DBPass string
	DBName string
	DBSSL  string

	RedisURL string

	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3Secure    bool
	S3Region    string

	// Terraform provider registry signing. When TFSigningKeyPath points at a
	// readable armored GPG private key, artifactapi serves local terraform
	// repos as a real provider registry (service discovery + signed
	// SHA256SUMS). Left empty, the registry endpoints stay disabled.
	TFSigningKeyPath       string
	TFSigningKeyPassphrase string
	TFProviderProtocols    string

	// github_rpm background syncer. The syncer keeps derived RPM metadata for
	// every github_rpm remote fresh off the client request path, sharing a
	// single global token-bucket limiter across all remotes so GitHub is never
	// hammered. Defaults are conservative: 1 req/s (3600/hr) sits well under an
	// authenticated token's 5000/hr. Unauthenticated remotes (60/hr) lean on
	// ETag/304 — an unchanged repo costs nothing — so keep those repos small or
	// configure a token.
	GitHubSyncRatePerSec   float64
	GitHubSyncBurst        int
	GitHubSyncWorkers      int
	GitHubSyncPollInterval int

	// Server-level GitHub machine credential, applied by default to every
	// outbound GitHub request (releases scan, ranged asset fetches, and the
	// generic-github byte proxy for private assets). Delivered via env/secret
	// only — never stored per-remote, never returned by an API, never logged.
	// Configure exactly one mode: a Personal Access Token, or a GitHub App
	// (id + installation id + private key). Partial App config fails at startup.
	GitHubToken             string
	GitHubAppID             string
	GitHubAppInstallationID string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyPath string
}

func (c *Config) DatabaseDSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.DBUser, c.DBPass, c.DBHost, c.DBPort, c.DBName, c.DBSSL,
	)
}

func Load() (*Config, error) {
	dbPort, err := strconv.Atoi(getenv("DBPORT", "5432"))
	if err != nil {
		return nil, fmt.Errorf("invalid DBPORT: %w", err)
	}

	s3Secure, _ := strconv.ParseBool(getenv("MINIO_SECURE", "false"))

	syncRate, err := strconv.ParseFloat(getenv("GITHUB_SYNC_RATE", "1"), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid GITHUB_SYNC_RATE: %w", err)
	}
	syncBurst, err := strconv.Atoi(getenv("GITHUB_SYNC_BURST", "5"))
	if err != nil {
		return nil, fmt.Errorf("invalid GITHUB_SYNC_BURST: %w", err)
	}
	syncWorkers, err := strconv.Atoi(getenv("GITHUB_SYNC_WORKERS", "3"))
	if err != nil {
		return nil, fmt.Errorf("invalid GITHUB_SYNC_WORKERS: %w", err)
	}
	syncPoll, err := strconv.Atoi(getenv("GITHUB_SYNC_POLL_INTERVAL", "60"))
	if err != nil {
		return nil, fmt.Errorf("invalid GITHUB_SYNC_POLL_INTERVAL: %w", err)
	}

	cfg := &Config{
		ListenAddr: getenv("LISTEN_ADDR", ":8000"),

		DBHost: getenv("DBHOST", "localhost"),
		DBPort: dbPort,
		DBUser: getenv("DBUSER", "artifacts"),
		DBPass: getenv("DBPASS", ""),
		DBName: getenv("DBNAME", "artifacts"),
		DBSSL:  getenv("DBSSL", "disable"),

		RedisURL: getenv("REDIS_URL", "redis://localhost:6379"),

		S3Endpoint:  getenv("MINIO_ENDPOINT", "localhost:9000"),
		S3AccessKey: getenv("MINIO_ACCESS_KEY", ""),
		S3SecretKey: getenv("MINIO_SECRET_KEY", ""),
		S3Bucket:    getenv("MINIO_BUCKET", "artifacts"),
		S3Secure:    s3Secure,
		S3Region:    getenv("MINIO_REGION", ""),

		TFSigningKeyPath:       getenv("TF_SIGNING_KEY_PATH", ""),
		TFSigningKeyPassphrase: getenv("TF_SIGNING_KEY_PASSPHRASE", ""),
		TFProviderProtocols:    getenv("TF_PROVIDER_PROTOCOLS", "5.0,6.0"),

		GitHubSyncRatePerSec:   syncRate,
		GitHubSyncBurst:        syncBurst,
		GitHubSyncWorkers:      syncWorkers,
		GitHubSyncPollInterval: syncPoll,

		GitHubToken:             getenv("GITHUB_TOKEN", ""),
		GitHubAppID:             getenv("GITHUB_APP_ID", ""),
		GitHubAppInstallationID: getenv("GITHUB_APP_INSTALLATION_ID", ""),
		GitHubAppPrivateKey:     getenv("GITHUB_APP_PRIVATE_KEY", ""),
		GitHubAppPrivateKeyPath: getenv("GITHUB_APP_PRIVATE_KEY_PATH", ""),
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
