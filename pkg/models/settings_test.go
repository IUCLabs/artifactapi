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
	ok := Settings{S3HealthUpThreshold: intp(1), S3HealthDownThreshold: intp(5), S3HealthIntervalMs: intp(2), S3HealthTimeoutMs: intp(1)}
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

func TestSettingsValidateTimeoutBelowInterval(t *testing.T) {
	if err := (Settings{S3HealthIntervalMs: intp(2000), S3HealthTimeoutMs: intp(2000)}).Validate(); err == nil {
		t.Error("timeout equal to interval validated")
	}
	if err := (Settings{S3HealthIntervalMs: intp(2000), S3HealthTimeoutMs: intp(1999)}).Validate(); err != nil {
		t.Errorf("timeout below interval: %v", err)
	}
	if err := (Settings{S3HealthTimeoutMs: intp(5000)}).Validate(); err != nil {
		t.Errorf("timeout alone cannot be compared: %v", err)
	}
}

func TestSettingsOverlay(t *testing.T) {
	base := Settings{S3HealthUpThreshold: intp(3), S3HealthDownThreshold: intp(3)}
	got := base.Overlay(Settings{S3HealthDownThreshold: intp(7), S3HealthIntervalMs: intp(500)})
	if *got.S3HealthUpThreshold != 3 || *got.S3HealthDownThreshold != 7 || *got.S3HealthIntervalMs != 500 || got.S3HealthTimeoutMs != nil {
		t.Fatalf("overlay = %+v", got)
	}
	if *base.S3HealthDownThreshold != 3 {
		t.Error("overlay mutated its receiver")
	}
}
