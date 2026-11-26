package models

import (
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

func TestSettingsValidate(t *testing.T) {
	if err := (&Settings{}).Validate(); err != nil {
		t.Fatalf("empty settings: %v", err)
	}
	ok := Settings{S3HealthUpThreshold: intp(1), S3HealthDownThreshold: intp(5), S3HealthIntervalMs: intp(1), S3HealthTimeoutMs: intp(1)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("minimum values: %v", err)
	}
	for name, s := range map[string]Settings{
		"s3_health_up_threshold":   {S3HealthUpThreshold: intp(0)},
		"s3_health_down_threshold": {S3HealthDownThreshold: intp(-1)},
		"s3_health_interval_ms":    {S3HealthIntervalMs: intp(0)},
		"s3_health_timeout_ms":     {S3HealthTimeoutMs: intp(0)},
	} {
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s below 1: err = %v, want one naming the field", name, err)
		}
	}
}
