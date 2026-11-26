package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Settings is the singleton global settings resource. Every field is a nullable override of an
// env default; a new setting needs a settings column, a pointer field here and its validation.
type Settings struct {
	S3HealthUpThreshold   *int `json:"s3_health_up_threshold"`
	S3HealthDownThreshold *int `json:"s3_health_down_threshold"`
	S3HealthIntervalMs    *int `json:"s3_health_interval_ms"`
	S3HealthTimeoutMs     *int `json:"s3_health_timeout_ms"`
}

// Overlay returns s with every non-nil field of o applied over it.
func (s Settings) Overlay(o Settings) Settings {
	dst, src := reflect.ValueOf(&s).Elem(), reflect.ValueOf(o)
	for i := range src.NumField() {
		if f := src.Field(i); !f.IsNil() {
			dst.Field(i).Set(f)
		}
	}
	return s
}

// Patch applies a JSON merge patch: a given value sets its field, an explicit null clears it,
// an absent key is kept, and an unknown key is an error.
func (s Settings) Patch(raw map[string]json.RawMessage) (Settings, error) {
	dst := reflect.ValueOf(&s).Elem()
	fields := map[string]reflect.Value{}
	for i := range dst.NumField() {
		fields[strings.Split(dst.Type().Field(i).Tag.Get("json"), ",")[0]] = dst.Field(i)
	}
	for k, v := range raw {
		f, ok := fields[k]
		if !ok {
			return s, fmt.Errorf("unknown setting %q", k)
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			f.SetZero()
			continue
		}
		p := reflect.New(f.Type())
		if err := json.Unmarshal(v, p.Interface()); err != nil {
			return s, fmt.Errorf("%s: %w", k, err)
		}
		f.Set(p.Elem())
	}
	return s, nil
}

// Validate rejects any set field below 1, and a set timeout not shorter than a set interval.
func (s Settings) Validate() error {
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
	if s.S3HealthIntervalMs != nil && s.S3HealthTimeoutMs != nil && *s.S3HealthTimeoutMs >= *s.S3HealthIntervalMs {
		return fmt.Errorf("s3_health_timeout_ms (%d) must be shorter than s3_health_interval_ms (%d)", *s.S3HealthTimeoutMs, *s.S3HealthIntervalMs)
	}
	return nil
}
