package database

import (
	"errors"
	"sync"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func intp(v int) *int { return &v }

func TestSettingsRoundTrip(t *testing.T) {
	requireDB(t)
	t.Cleanup(func() { _ = testDB.ResetSettings(ctx()) })

	if _, err := testDB.Pool.Exec(ctx(), "DELETE FROM settings"); err != nil {
		t.Fatal(err)
	}
	got, err := testDB.GetSettings(ctx())
	if err != nil || got != (models.Settings{}) {
		t.Fatalf("no row: got %+v, %v; want all nil", got, err)
	}

	if err := testDB.PutSettings(ctx(), models.Settings{S3HealthUpThreshold: intp(2), S3HealthIntervalMs: intp(5000)}); err != nil {
		t.Fatal(err)
	}
	got, err = testDB.GetSettings(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if got.S3HealthUpThreshold == nil || *got.S3HealthUpThreshold != 2 || got.S3HealthIntervalMs == nil || *got.S3HealthIntervalMs != 5000 {
		t.Fatalf("after put: %+v", got)
	}
	if got.S3HealthDownThreshold != nil || got.S3HealthTimeoutMs != nil {
		t.Fatalf("unset fields must stay nil: %+v", got)
	}

	if err := testDB.PutSettings(ctx(), models.Settings{S3HealthDownThreshold: intp(4)}); err != nil {
		t.Fatal(err)
	}
	got, _ = testDB.GetSettings(ctx())
	if got.S3HealthUpThreshold != nil || got.S3HealthDownThreshold == nil || *got.S3HealthDownThreshold != 4 {
		t.Fatalf("put must replace every field: %+v", got)
	}

	if err := testDB.ResetSettings(ctx()); err != nil {
		t.Fatal(err)
	}
	if got, _ = testDB.GetSettings(ctx()); got != (models.Settings{}) {
		t.Fatalf("after reset: %+v, want all nil", got)
	}
}

func TestSettingsRejectsOutOfBounds(t *testing.T) {
	requireDB(t)
	t.Cleanup(func() { _ = testDB.ResetSettings(ctx()) })
	for name, s := range map[string]models.Settings{
		"zero threshold":      {S3HealthDownThreshold: intp(0)},
		"threshold over 100":  {S3HealthUpThreshold: intp(101)},
		"interval under 500":  {S3HealthIntervalMs: intp(499)},
		"interval over 1h":    {S3HealthIntervalMs: intp(3_600_001)},
		"timeout under 100":   {S3HealthTimeoutMs: intp(99)},
		"timeout >= interval": {S3HealthIntervalMs: intp(1000), S3HealthTimeoutMs: intp(1000)},
	} {
		if err := testDB.PutSettings(ctx(), s); err == nil {
			t.Errorf("%s must violate a column check", name)
		}
	}
}

func TestUpdateSettingsSerialises(t *testing.T) {
	requireDB(t)
	t.Cleanup(func() { _ = testDB.ResetSettings(ctx()) })
	if _, err := testDB.Pool.Exec(ctx(), "DELETE FROM settings"); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			errs <- testDB.UpdateSettings(ctx(), func(s models.Settings) (models.Settings, error) {
				v := 1
				if s.S3HealthUpThreshold != nil {
					v = *s.S3HealthUpThreshold + 1
				}
				s.S3HealthUpThreshold = &v
				return s, nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := testDB.GetSettings(ctx())
	if err != nil || got.S3HealthUpThreshold == nil || *got.S3HealthUpThreshold != n {
		t.Fatalf("after %d concurrent increments: %+v, %v", n, got, err)
	}

	abort := errors.New("abort")
	if err := testDB.UpdateSettings(ctx(), func(models.Settings) (models.Settings, error) {
		return models.Settings{}, abort
	}); !errors.Is(err, abort) {
		t.Fatalf("fn error = %v, want %v", err, abort)
	}
	if got, _ = testDB.GetSettings(ctx()); got.S3HealthUpThreshold == nil || *got.S3HealthUpThreshold != n {
		t.Fatalf("an aborted update changed the row: %+v", got)
	}
}
