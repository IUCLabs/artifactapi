package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Unset the vars Load reads so the fallback defaults are exercised.
	for _, k := range []string{
		"LISTEN_ADDR", "DBHOST", "DBPORT", "DBUSER", "DBPASS", "DBNAME", "DBSSL",
		"REDIS_URL", "MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY",
		"MINIO_BUCKET", "MINIO_SECURE", "MINIO_REGION",
	} {
		old, ok := os.LookupEnv(k)
		os.Unsetenv(k)
		if ok {
			t.Cleanup(func() { os.Setenv(k, old) })
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ListenAddr != ":8000" || cfg.DBPort != 5432 || cfg.DBUser != "artifacts" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.RedisURL != "redis://localhost:6379" || cfg.S3Bucket != "artifacts" || cfg.S3Secure {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("DBHOST", "db.example.com")
	t.Setenv("DBPORT", "6000")
	t.Setenv("DBUSER", "u")
	t.Setenv("DBPASS", "pw")
	t.Setenv("DBNAME", "n")
	t.Setenv("DBSSL", "require")
	t.Setenv("MINIO_SECURE", "true")
	t.Setenv("MINIO_REGION", "us-east-1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ListenAddr != ":9999" || cfg.DBHost != "db.example.com" || cfg.DBPort != 6000 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if !cfg.S3Secure {
		t.Error("MINIO_SECURE=true not parsed")
	}
	want := "postgres://u:pw@db.example.com:6000/n?sslmode=require"
	if got := cfg.DatabaseDSN(); got != want {
		t.Errorf("DSN = %q, want %q", got, want)
	}
}

func TestLoadInvalidPort(t *testing.T) {
	t.Setenv("DBPORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Error("expected error for invalid DBPORT")
	}
}
