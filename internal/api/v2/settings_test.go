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
func (f *fakeSettings) ResetSettings(context.Context) error {
	if f.err != nil {
		return f.err
	}
	f.s = models.Settings{}
	return nil
}

var settingsDefaults = proxy.ProbeConfig{UpThreshold: 3, DownThreshold: 3, Interval: 3 * time.Second, Timeout: 1500 * time.Millisecond}

type settingsBody struct {
	Up       int `json:"s3_health_up_threshold"`
	Down     int `json:"s3_health_down_threshold"`
	Interval int `json:"s3_health_interval_ms"`
	Timeout  int `json:"s3_health_timeout_ms"`
}

func settingsCall(t *testing.T, store *fakeSettings, method, body string) (int, settingsBody) {
	t.Helper()
	h := NewSettingsHandler(store, settingsDefaults).Routes()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out settingsBody
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, out
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

func TestSettingsPutRejectsInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"zero up threshold":          `{"s3_health_up_threshold":0}`,
		"negative down threshold":    `{"s3_health_down_threshold":-2}`,
		"interval below env timeout": `{"s3_health_interval_ms":1000}`,
		"timeout equal to interval":  `{"s3_health_interval_ms":2000,"s3_health_timeout_ms":2000}`,
		"not json":                   `nope`,
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
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if code, _ := settingsCall(t, store, m, `{}`); code != 500 {
			t.Errorf("%s with store error = %d, want 500", m, code)
		}
	}
}
