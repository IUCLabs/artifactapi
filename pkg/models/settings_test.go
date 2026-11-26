package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

func TestSettingsValidate(t *testing.T) {
	if err := (&Settings{}).Validate(); err != nil {
		t.Fatalf("empty settings: %v", err)
	}
	lowest := Settings{S3HealthUpThreshold: intp(1), S3HealthDownThreshold: intp(1), S3HealthIntervalMs: intp(500), S3HealthTimeoutMs: intp(100)}
	if err := lowest.Validate(); err != nil {
		t.Fatalf("minimum values: %v", err)
	}
	highest := Settings{S3HealthUpThreshold: intp(100), S3HealthDownThreshold: intp(100), S3HealthIntervalMs: intp(3_600_000), S3HealthTimeoutMs: intp(3_599_999)}
	if err := highest.Validate(); err != nil {
		t.Fatalf("maximum values: %v", err)
	}
	for name, cases := range map[string][]Settings{
		"s3_health_up_threshold":   {{S3HealthUpThreshold: intp(0)}, {S3HealthUpThreshold: intp(101)}},
		"s3_health_down_threshold": {{S3HealthDownThreshold: intp(-1)}, {S3HealthDownThreshold: intp(101)}},
		"s3_health_interval_ms":    {{S3HealthIntervalMs: intp(499)}, {S3HealthIntervalMs: intp(3_600_001)}, {S3HealthIntervalMs: intp(1 << 32)}},
		"s3_health_timeout_ms":     {{S3HealthTimeoutMs: intp(99)}, {S3HealthTimeoutMs: intp(3_600_000)}},
	} {
		for _, s := range cases {
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s out of bounds in %+v: err = %v, want one naming the field", name, s, err)
			}
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

func TestSettingsPatch(t *testing.T) {
	base := Settings{S3HealthUpThreshold: intp(2), S3HealthDownThreshold: intp(4)}
	got, err := base.Patch(map[string]json.RawMessage{
		"s3_health_up_threshold": json.RawMessage(" null "),
		"s3_health_interval_ms":  json.RawMessage("5000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.S3HealthUpThreshold != nil || *got.S3HealthDownThreshold != 4 || *got.S3HealthIntervalMs != 5000 || got.S3HealthTimeoutMs != nil {
		t.Fatalf("patch = %+v", got)
	}
	if base.S3HealthUpThreshold == nil || base.S3HealthIntervalMs != nil {
		t.Error("patch mutated its receiver")
	}

	for name, raw := range map[string]map[string]json.RawMessage{
		"unknown key": {"s3_health_up_treshold": json.RawMessage("2")},
		"wrong type":  {"s3_health_up_threshold": json.RawMessage(`"2"`)},
	} {
		if _, err := base.Patch(raw); err == nil {
			t.Errorf("%s: patch accepted", name)
		}
	}
}
