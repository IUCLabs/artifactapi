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
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
