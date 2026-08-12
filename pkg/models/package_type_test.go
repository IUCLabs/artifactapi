package models_test

import (
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestPackageTypeValid(t *testing.T) {
	valid := []models.PackageType{
		models.PackageGeneric,
		models.PackageDocker,
		models.PackageHelm,
		models.PackagePyPI,
		models.PackageNPM,
		models.PackageRPM,
		models.PackageAlpine,
		models.PackagePuppet,
		models.PackageTerraform,
		models.PackageGoProxy,
		models.PackageGitHubRPM,
		models.PackageGitHubDeb,
		models.PackageGitHubAlpine,
	}
	for _, pt := range valid {
		if !pt.Valid() {
			t.Errorf("expected %q to be valid", pt)
		}
	}
}

func TestPackageTypeInvalid(t *testing.T) {
	invalid := []string{"", "bogus", "Docker", "HELM"}
	for _, s := range invalid {
		pt := models.PackageType(s)
		if pt.Valid() {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestParsePackageType(t *testing.T) {
	pt, err := models.ParsePackageType("docker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pt != models.PackageDocker {
		t.Errorf("expected docker, got %q", pt)
	}

	_, err = models.ParsePackageType("nope")
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestPackageTypeString(t *testing.T) {
	if models.PackageGoProxy.String() != "goproxy" {
		t.Errorf("expected 'goproxy', got %q", models.PackageGoProxy.String())
	}
}
