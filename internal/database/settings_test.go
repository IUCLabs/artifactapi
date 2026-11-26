package database

import (
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

func TestSettingsRejectsNonPositive(t *testing.T) {
	requireDB(t)
	t.Cleanup(func() { _ = testDB.ResetSettings(ctx()) })
	if err := testDB.PutSettings(ctx(), models.Settings{S3HealthDownThreshold: intp(0)}); err == nil {
		t.Fatal("a zero threshold must violate the column check")
	}
}
