package rpm_test

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/rpm"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestProvider_Type(t *testing.T) {
	p := &rpm.Provider{}
	if p.Type() != models.PackageRPM {
		t.Errorf("expected rpm, got %q", p.Type())
	}
}

func TestProvider_Classify(t *testing.T) {
	p := &rpm.Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"repomd.xml", provider.Mutable},
		{"repodata/primary.xml.gz", provider.Mutable},
		{"Packages.gz", provider.Mutable},
		{"package-1.0.rpm", provider.Immutable},
		{"RPM-GPG-KEY-almalinux", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
