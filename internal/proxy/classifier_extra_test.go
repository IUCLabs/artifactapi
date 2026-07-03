package proxy

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	_ "github.com/IUCLabs/artifactapi/internal/provider/generic"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestClassifierBranches(t *testing.T) {
	gp, err := provider.Get(models.PackageGeneric)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClassifier(gp)

	if c.Classify(models.Remote{Blocklist: []string{`\.exe$`}}, "x.exe") != ClassDenied {
		t.Error("blocklist match should be denied")
	}
	// Allowlist present but path doesn't match -> denied.
	allow := models.Remote{Patterns: []string{`^allowed/`}}
	if c.Classify(allow, "other/x") != ClassDenied {
		t.Error("non-allowlisted path should be denied")
	}
	if c.Classify(allow, "allowed/x") != ClassImmutable {
		t.Error("allowlisted generic path should be immutable")
	}
	if c.Classify(models.Remote{MutablePatterns: []string{`index$`}}, "a/index") != ClassMutable {
		t.Error("mutable pattern override failed")
	}
	if c.Classify(models.Remote{ImmutablePatterns: []string{`\.bin$`}}, "a.bin") != ClassImmutable {
		t.Error("immutable pattern failed")
	}
	// An invalid regex is skipped (not treated as a match) rather than denying.
	if c.Classify(models.Remote{Blocklist: []string{`[invalid`}}, "anything") == ClassDenied {
		t.Error("invalid blocklist regex should be skipped, not deny everything")
	}
}

func TestClassificationString(t *testing.T) {
	for c, want := range map[Classification]string{
		ClassImmutable:     "immutable",
		ClassMutable:       "mutable",
		ClassDenied:        "denied",
		Classification(99): "unknown",
	} {
		if c.String() != want {
			t.Errorf("Classification(%d).String() = %q, want %q", c, c.String(), want)
		}
	}
}
