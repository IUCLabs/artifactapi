package keycrypt

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Config selects and parameterises the private key cipher.
type Config struct {
	Mode      string
	StaticKey []byte
	Vault     VaultConfig
}

// VaultConfig configures transit mode with Vault Kubernetes auth.
type VaultConfig struct {
	Addr         string
	Role         string
	K8sMount     string
	TokenPath    string
	TransitMount string
	TransitKey   string
	CACert       string
}

// Default paths for transit mode.
const (
	DefaultK8sMount     = "kubernetes"
	DefaultTokenPath    = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultTransitMount = "transit"
)

// ConfigFromEnv reads GITHUB_APP_KEY_ENCRYPTION* and VAULT_* via getenv
// (which returns def when the variable is unset) and validates the result.
func ConfigFromEnv(getenv func(key, def string) string) (Config, error) {
	cfg := Config{
		Mode: strings.TrimSpace(getenv("GITHUB_APP_KEY_ENCRYPTION", ModeNone)),
		Vault: VaultConfig{
			Addr:         getenv("VAULT_ADDR", ""),
			Role:         getenv("VAULT_K8S_ROLE", ""),
			K8sMount:     getenv("VAULT_K8S_MOUNT", DefaultK8sMount),
			TokenPath:    getenv("VAULT_K8S_TOKEN_PATH", DefaultTokenPath),
			TransitMount: getenv("VAULT_TRANSIT_MOUNT", DefaultTransitMount),
			TransitKey:   getenv("VAULT_TRANSIT_KEY", ""),
			CACert:       getenv("VAULT_CACERT", ""),
		},
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeNone
	}
	if raw := getenv("GITHUB_APP_KEY_ENCRYPTION_KEY", ""); raw != "" {
		if cfg.Mode != ModeStatic {
			return Config{}, fmt.Errorf("GITHUB_APP_KEY_ENCRYPTION_KEY is set but GITHUB_APP_KEY_ENCRYPTION=%s", cfg.Mode)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			return Config{}, errors.New("GITHUB_APP_KEY_ENCRYPTION_KEY must be standard base64")
		}
		cfg.StaticKey = key
	}
	return cfg, cfg.Validate()
}

// Validate rejects unknown modes and missing or malformed mode settings.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeNone, "":
		return nil
	case ModeStatic:
		if len(c.StaticKey) != 32 {
			return errors.New("GITHUB_APP_KEY_ENCRYPTION=static requires GITHUB_APP_KEY_ENCRYPTION_KEY: base64 of exactly 32 bytes")
		}
		return nil
	case ModeTransit:
		return c.Vault.validate()
	}
	return fmt.Errorf("invalid GITHUB_APP_KEY_ENCRYPTION %q: want none, static or transit", c.Mode)
}

func (v VaultConfig) validate() error {
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"VAULT_ADDR", v.Addr}, {"VAULT_K8S_ROLE", v.Role}, {"VAULT_K8S_MOUNT", v.K8sMount},
		{"VAULT_K8S_TOKEN_PATH", v.TokenPath}, {"VAULT_TRANSIT_MOUNT", v.TransitMount}, {"VAULT_TRANSIT_KEY", v.TransitKey},
	} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("GITHUB_APP_KEY_ENCRYPTION=transit requires %s", strings.Join(missing, ", "))
	}
	u, err := url.Parse(v.Addr)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("invalid VAULT_ADDR %q: want an http(s) URL", v.Addr)
	}
	for _, f := range []struct{ name, val string }{
		{"VAULT_K8S_MOUNT", v.K8sMount}, {"VAULT_TRANSIT_MOUNT", v.TransitMount}, {"VAULT_TRANSIT_KEY", v.TransitKey},
	} {
		if strings.ContainsAny(f.val, "?#") || strings.Contains(f.val, "..") || (f.name == "VAULT_TRANSIT_KEY" && strings.Contains(f.val, "/")) {
			return fmt.Errorf("invalid %s %q", f.name, f.val)
		}
	}
	return nil
}
