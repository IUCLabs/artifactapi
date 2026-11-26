package v2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type fakeSettings struct {
	s   models.Settings
	err error
	put int
}

func (f *fakeSettings) GetSettings(context.Context) (models.Settings, error) { return f.s, f.err }
func (f *fakeSettings) PutSettings(_ context.Context, s models.Settings) error {
	if f.err != nil {
		return f.err
	}
	f.put++
	f.s = s
	return nil
}
func (f *fakeSettings) UpdateSettings(_ context.Context, fn func(models.Settings) (models.Settings, error)) error {
	if f.err != nil {
		return f.err
	}
	next, err := fn(f.s)
	if err != nil {
		return err
	}
	f.put++
	f.s = next
	return nil
}
func (f *fakeSettings) ResetSettings(context.Context) error {
	if f.err != nil {
		return f.err
	}
	f.s = models.Settings{}
	return nil
}

var settingsDefaults = proxy.ProbeConfig{UpThreshold: 3, DownThreshold: 3, Interval: 3 * time.Second, Timeout: 1500 * time.Millisecond}.Settings()

type settingsBody struct {
	Up       int `json:"s3_health_up_threshold"`
	Down     int `json:"s3_health_down_threshold"`
	Interval int `json:"s3_health_interval_ms"`
	Timeout  int `json:"s3_health_timeout_ms"`
}

type settingsValidity struct {
	Valid bool   `json:"valid"`
	Error string `json:"error"`
}

func settingsCall(t *testing.T, store *fakeSettings, method, body string) (int, settingsBody) {
	t.Helper()
	code, out, _ := settingsCallWith(t, store, settingsDefaults, method, body)
	return code, out
}

func settingsCallWith(t *testing.T, store *fakeSettings, defaults models.Settings, method, body string) (int, settingsBody, settingsValidity) {
	t.Helper()
	h := NewSettingsHandler(store, defaults).Routes()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out settingsBody
	var v settingsValidity
	if w.Code == http.StatusOK {
		raw := w.Body.Bytes()
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, out, v
}

func TestSettingsGetReturnsEffectiveValues(t *testing.T) {
	store := &fakeSettings{}
	code, got := settingsCall(t, store, "GET", "")
	if code != 200 || got != (settingsBody{3, 3, 3000, 1500}) {
		t.Fatalf("defaults: %d %+v", code, got)
	}
	five := 5
	store.s.S3HealthDownThreshold = &five
	if _, got = settingsCall(t, store, "GET", ""); got != (settingsBody{3, 5, 3000, 1500}) {
		t.Fatalf("with override: %+v", got)
	}
}

func TestSettingsPut(t *testing.T) {
	store := &fakeSettings{}
	code, got := settingsCall(t, store, "PUT", `{"s3_health_up_threshold":1,"s3_health_down_threshold":6}`)
	if code != 200 || got != (settingsBody{1, 6, 3000, 1500}) {
		t.Fatalf("put: %d %+v", code, got)
	}
	if store.s.S3HealthIntervalMs != nil || store.s.S3HealthTimeoutMs != nil {
		t.Errorf("omitted fields must be stored unset to track env defaults: %+v", store.s)
	}

	code, got = settingsCall(t, store, "PUT", `{"s3_health_down_threshold":2}`)
	if code != 200 || got != (settingsBody{3, 2, 3000, 1500}) {
		t.Fatalf("put replaces, omitted up reverts to default: %d %+v", code, got)
	}
}

func TestSettingsPatchKeepsUnsetFields(t *testing.T) {
	store := &fakeSettings{}
	if code, _ := settingsCall(t, store, "PUT", `{"s3_health_up_threshold":1}`); code != 200 {
		t.Fatalf("put: %d", code)
	}
	code, got := settingsCall(t, store, "PATCH", `{"s3_health_down_threshold":6}`)
	if code != 200 || got != (settingsBody{1, 6, 3000, 1500}) {
		t.Fatalf("patch: %d %+v", code, got)
	}
	if store.s.S3HealthUpThreshold == nil || *store.s.S3HealthUpThreshold != 1 || store.s.S3HealthIntervalMs != nil {
		t.Errorf("patch must keep stored overrides and leave defaults unset: %+v", store.s)
	}
}

func TestSettingsPatchValidatesMergedResult(t *testing.T) {
	timeout := 2500
	store := &fakeSettings{s: models.Settings{S3HealthTimeoutMs: &timeout}}
	if code, _ := settingsCall(t, store, "PATCH", `{"s3_health_interval_ms":2000}`); code != 400 {
		t.Fatalf("patch making interval <= stored timeout: %d, want 400", code)
	}
	if store.put != 0 {
		t.Error("an invalid patch reached the store")
	}
}

func TestSettingsPutRejectsInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"zero up threshold":          `{"s3_health_up_threshold":0}`,
		"negative down threshold":    `{"s3_health_down_threshold":-2}`,
		"interval below env timeout": `{"s3_health_interval_ms":1000}`,
		"timeout equal to interval":  `{"s3_health_interval_ms":2000,"s3_health_timeout_ms":2000}`,
		"not json":                   `nope`,
		"unknown field":              `{"s3_health_up_treshold":2}`,
		"threshold above 100":        `{"s3_health_down_threshold":101}`,
		"interval below 500ms":       `{"s3_health_interval_ms":499,"s3_health_timeout_ms":100}`,
		"interval above 1h":          `{"s3_health_interval_ms":3600001}`,
		"interval beyond int32":      `{"s3_health_interval_ms":4294967296}`,
		"timeout below 100ms":        `{"s3_health_timeout_ms":99}`,
		"trailing value":             `{"s3_health_up_threshold":2}{"s3_health_up_threshold":3}`,
		"trailing garbage":           `{"s3_health_up_threshold":2} x`,
	} {
		store := &fakeSettings{}
		if code, _ := settingsCall(t, store, "PUT", body); code != 400 {
			t.Errorf("%s: status %d, want 400", name, code)
		}
		if store.put != 0 {
			t.Errorf("%s: invalid settings reached the store", name)
		}
	}
}

func TestSettingsDeleteResets(t *testing.T) {
	two := 2
	store := &fakeSettings{s: models.Settings{S3HealthUpThreshold: &two}}
	if code, _ := settingsCall(t, store, "DELETE", ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, got := settingsCall(t, store, "GET", ""); got != (settingsBody{3, 3, 3000, 1500}) {
		t.Fatalf("after reset: %+v", got)
	}
}

func TestSettingsStoreErrors(t *testing.T) {
	store := &fakeSettings{err: errors.New("db down")}
	for _, m := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		if code, _ := settingsCall(t, store, m, `{}`); code != 500 {
			t.Errorf("%s with store error = %d, want 500", m, code)
		}
	}
}

func TestSettingsPatchNullClearsOverride(t *testing.T) {
	store := &fakeSettings{}
	if code, _ := settingsCall(t, store, "PUT", `{"s3_health_up_threshold":1,"s3_health_down_threshold":6}`); code != 200 {
		t.Fatalf("put: %d", code)
	}
	code, got := settingsCall(t, store, "PATCH", `{"s3_health_up_threshold":null}`)
	if code != 200 || got != (settingsBody{3, 6, 3000, 1500}) {
		t.Fatalf("patch null: %d %+v", code, got)
	}
	if store.s.S3HealthUpThreshold != nil || store.s.S3HealthDownThreshold == nil || *store.s.S3HealthDownThreshold != 6 {
		t.Errorf("null must clear only its override: %+v", store.s)
	}
}

func TestSettingsPatchRejectsUnknownAndMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `{"s3_health_up_treshold":2}`,
		"wrong type":    `{"s3_health_up_threshold":"2"}`,
		"not an object": `[1]`,
		"not json":      `nope`,
		"out of range":  `{"s3_health_up_threshold":101}`,
		"trailing data": `{"s3_health_up_threshold":2}{}`,
	} {
		one := 1
		store := &fakeSettings{s: models.Settings{S3HealthUpThreshold: &one}}
		if code, _ := settingsCall(t, store, "PATCH", body); code != 400 {
			t.Errorf("%s: status %d, want 400", name, code)
		}
		if store.put != 0 || store.s.S3HealthUpThreshold != &one {
			t.Errorf("%s: a rejected patch changed the store", name)
		}
	}
}

func TestSettingsGetReportsValidity(t *testing.T) {
	store := &fakeSettings{}
	if code, _, v := settingsCallWith(t, store, settingsDefaults, "GET", ""); code != 200 || !v.Valid || v.Error != "" {
		t.Fatalf("defaults: %d %+v", code, v)
	}

	timeout := 2500
	store.s.S3HealthTimeoutMs = &timeout
	if code, _, v := settingsCallWith(t, store, settingsDefaults, "GET", ""); code != 200 || !v.Valid {
		t.Fatalf("timeout below env interval: %d %+v", code, v)
	}
	shorter := proxy.ProbeConfig{UpThreshold: 3, DownThreshold: 3, Interval: 2 * time.Second, Timeout: time.Second}.Settings()
	code, got, v := settingsCallWith(t, store, shorter, "GET", "")
	if code != 200 || got != (settingsBody{3, 3, 2000, 2500}) {
		t.Fatalf("stored timeout over a shrunk env interval: %d %+v", code, got)
	}
	if v.Valid || !strings.Contains(v.Error, "s3_health_timeout_ms") {
		t.Fatalf("validity = %+v, want invalid naming the timeout", v)
	}
}

func TestSettingsResponsesCarryOverrides(t *testing.T) {
	store := &fakeSettings{}
	wantKeys := []string{"s3_health_up_threshold", "s3_health_down_threshold", "s3_health_interval_ms", "s3_health_timeout_ms"}
	call := func(method, body string) map[string]*int {
		t.Helper()
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		NewSettingsHandler(store, settingsDefaults).Routes().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body)
		}
		var out struct {
			Overrides map[string]*int `json:"overrides"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, k := range wantKeys {
			if _, ok := out.Overrides[k]; !ok {
				t.Fatalf("%s: overrides missing %q: %v", method, k, out.Overrides)
			}
		}
		return out.Overrides
	}
	unset := func(o map[string]*int, keys ...string) bool {
		for _, k := range keys {
			if o[k] != nil {
				return false
			}
		}
		return true
	}

	if o := call("GET", ""); !unset(o, wantKeys...) {
		t.Fatalf("no overrides stored: %v", o)
	}
	if o := call("PUT", `{"s3_health_down_threshold":6}`); o["s3_health_down_threshold"] == nil || *o["s3_health_down_threshold"] != 6 || !unset(o, "s3_health_up_threshold", "s3_health_interval_ms", "s3_health_timeout_ms") {
		t.Fatalf("put overrides: %v", o)
	}
	if o := call("PATCH", `{"s3_health_up_threshold":2}`); o["s3_health_up_threshold"] == nil || *o["s3_health_up_threshold"] != 2 || o["s3_health_down_threshold"] == nil {
		t.Fatalf("patch overrides: %v", o)
	}
	if o := call("PATCH", `{"s3_health_down_threshold":null}`); o["s3_health_down_threshold"] != nil || o["s3_health_up_threshold"] == nil {
		t.Fatalf("cleared override must read null: %v", o)
	}
	if o := call("GET", ""); *o["s3_health_up_threshold"] != 2 || !unset(o, "s3_health_down_threshold", "s3_health_interval_ms", "s3_health_timeout_ms") {
		t.Fatalf("get overrides: %v", o)
	}
}
