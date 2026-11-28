package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
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

	// Object store health probe: the state flips after this many consecutive like results.
	S3HealthUpThreshold   int
	S3HealthDownThreshold int
	S3HealthInterval      time.Duration
	S3HealthTimeout       time.Duration

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

	// GitHubAppAPIHosts are extra hostnames (GHES) an API-managed GitHub App's
	// api_base_url may target besides api.github.com.
	GitHubAppAPIHosts []string

	// Bearer-JWT auth on configuration writes; empty issuer disables it.
	OIDCIssuer    string
	OIDCAudiences []string
	// AuthUploads extends that auth to local repo uploads, deletes and evictions.
	AuthUploads bool
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

	healthUp, err := strconv.Atoi(getenv("S3_HEALTH_UP_THRESHOLD", "3"))
	if err != nil {
		return nil, fmt.Errorf("invalid S3_HEALTH_UP_THRESHOLD: must be an integer")
	}
	healthDown, err := strconv.Atoi(getenv("S3_HEALTH_DOWN_THRESHOLD", "3"))
	if err != nil {
		return nil, fmt.Errorf("invalid S3_HEALTH_DOWN_THRESHOLD: must be an integer")
	}
	healthInterval, err := wholeMillis("S3_HEALTH_INTERVAL", "3s")
	if err != nil {
		return nil, err
	}
	healthTimeout, err := wholeMillis("S3_HEALTH_TIMEOUT", "1.5s")
	if err != nil {
		return nil, err
	}
	intervalMs, timeoutMs := int(healthInterval.Milliseconds()), int(healthTimeout.Milliseconds())
	if err := (models.Settings{S3HealthUpThreshold: &healthUp, S3HealthDownThreshold: &healthDown, S3HealthIntervalMs: &intervalMs, S3HealthTimeoutMs: &timeoutMs}).Validate(); err != nil {
		return nil, fmt.Errorf("invalid S3_HEALTH_* settings: %w", err)
	}

	oidcIssuer := getenv("ARTIFACTAPI_OIDC_ISSUER", "")
	var oidcAudiences []string
	for _, a := range strings.Split(getenv("ARTIFACTAPI_OIDC_AUDIENCE", ""), ",") {
		if a = strings.TrimSpace(a); a != "" {
			oidcAudiences = append(oidcAudiences, a)
		}
	}
	if oidcIssuer != "" && len(oidcAudiences) == 0 {
		return nil, fmt.Errorf("ARTIFACTAPI_OIDC_AUDIENCE is required when ARTIFACTAPI_OIDC_ISSUER is set")
	}
	authUploads, err := strconv.ParseBool(getenv("ARTIFACTAPI_AUTH_UPLOADS", "false"))
	if err != nil {
		return nil, fmt.Errorf("invalid ARTIFACTAPI_AUTH_UPLOADS: %w", err)
	}
	if authUploads && oidcIssuer == "" {
		return nil, fmt.Errorf("ARTIFACTAPI_OIDC_ISSUER is required when ARTIFACTAPI_AUTH_UPLOADS is set")
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

		S3HealthUpThreshold:   healthUp,
		S3HealthDownThreshold: healthDown,
		S3HealthInterval:      healthInterval,
		S3HealthTimeout:       healthTimeout,

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
		GitHubAppAPIHosts:       splitHosts(getenv("GITHUB_APP_API_HOSTS", "")),

		OIDCIssuer:    oidcIssuer,
		OIDCAudiences: oidcAudiences,
		AuthUploads:   authUploads,
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func wholeMillis(key, def string) (time.Duration, error) {
	d, err := time.ParseDuration(getenv(key, def))
	if err != nil || d%time.Millisecond != 0 {
		return 0, fmt.Errorf("invalid %s: must be a duration in whole milliseconds", key)
	}
	return d, nil
}

// splitHosts parses a comma-separated hostname list, lowercased, blanks dropped.
func splitHosts(v string) []string {
	var hosts []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}
