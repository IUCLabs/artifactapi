package models

import "fmt"

// Settings holds runtime overrides; a nil field falls back to the env default.
type Settings struct {
	S3HealthUpThreshold   *int `json:"s3_health_up_threshold"`
	S3HealthDownThreshold *int `json:"s3_health_down_threshold"`
	S3HealthIntervalMs    *int `json:"s3_health_interval_ms"`
	S3HealthTimeoutMs     *int `json:"s3_health_timeout_ms"`
}

// Validate rejects any set field below 1.
func (s *Settings) Validate() error {
	for _, f := range []struct {
		name string
		v    *int
	}{
		{"s3_health_up_threshold", s.S3HealthUpThreshold},
		{"s3_health_down_threshold", s.S3HealthDownThreshold},
		{"s3_health_interval_ms", s.S3HealthIntervalMs},
		{"s3_health_timeout_ms", s.S3HealthTimeoutMs},
	} {
		if f.v != nil && *f.v < 1 {
			return fmt.Errorf("%s must be at least 1, got %d", f.name, *f.v)
		}
	}
	return nil
}
