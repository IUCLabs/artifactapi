package proxy_test

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider/docker"
	"github.com/IUCLabs/artifactapi/internal/provider/generic"
	"github.com/IUCLabs/artifactapi/internal/provider/helm"
	"github.com/IUCLabs/artifactapi/internal/provider/rpm"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestClassifier_EmptyPatternsAllowsAll(t *testing.T) {
	c := proxy.NewClassifier(&generic.Provider{})
	remote := models.Remote{Name: "test"}
	if c.Classify(remote, "any/path") == proxy.ClassDenied {
		t.Error("empty patterns should allow all paths")
	}
}

func TestClassifier_PatternsActAsAllowlist(t *testing.T) {
	c := proxy.NewClassifier(&generic.Provider{})
	remote := models.Remote{
		Name:     "test",
		Patterns: []string{`^releases/`},
	}
	if c.Classify(remote, "releases/v1.0/app.tar.gz") == proxy.ClassDenied {
		t.Error("path matching patterns should be allowed")
	}
	if c.Classify(remote, "uploads/other.tar.gz") != proxy.ClassDenied {
		t.Error("path not matching patterns should be denied")
	}
}

func TestClassifier_BlocklistDenies(t *testing.T) {
	c := proxy.NewClassifier(&generic.Provider{})
	remote := models.Remote{
		Name:      "test",
		Blocklist: []string{`\.exe$`},
	}
	if c.Classify(remote, "malware.exe") != proxy.ClassDenied {
		t.Error("blocklist match should deny")
	}
	if c.Classify(remote, "legit.tar.gz") == proxy.ClassDenied {
		t.Error("non-blocked path should be allowed")
	}
}

func TestClassifier_BlocklistBeforePatterns(t *testing.T) {
	c := proxy.NewClassifier(&generic.Provider{})
	remote := models.Remote{
		Name:      "test",
		Patterns:  []string{`^releases/`},
		Blocklist: []string{`releases/v0\.1/`},
	}
	if c.Classify(remote, "releases/v0.1/app.tar.gz") != proxy.ClassDenied {
		t.Error("blocklist should take priority")
	}
}

func TestClassifier_GenericAllImmutable(t *testing.T) {
	c := proxy.NewClassifier(&generic.Provider{})
	remote := models.Remote{Name: "test"}
	if c.Classify(remote, "any/file.tar.gz") != proxy.ClassImmutable {
		t.Error("generic provider should classify everything as immutable")
	}
}

func TestClassifier_GenericMutableOverride(t *testing.T) {
	c := proxy.NewClassifier(&generic.Provider{})
	remote := models.Remote{
		Name:            "test",
		MutablePatterns: []string{`/archive/refs/heads/`},
	}
	if c.Classify(remote, "repo/archive/refs/heads/main.tar.gz") != proxy.ClassMutable {
		t.Error("mutable_patterns should override provider default")
	}
	if c.Classify(remote, "repo/releases/v1.0.tar.gz") != proxy.ClassImmutable {
		t.Error("non-mutable path should stay immutable")
	}
}

func TestClassifier_ImmutableOverride(t *testing.T) {
	c := proxy.NewClassifier(&helm.Provider{})
	remote := models.Remote{
		Name:              "test",
		ImmutablePatterns: []string{`special-index\.yaml$`},
	}
	if c.Classify(remote, "special-index.yaml") != proxy.ClassImmutable {
		t.Error("immutable_patterns should force immutable even for normally mutable paths")
	}
}

func TestClassifier_HelmAutoClassifies(t *testing.T) {
	c := proxy.NewClassifier(&helm.Provider{})
	remote := models.Remote{Name: "test"}
	if c.Classify(remote, "index.yaml") != proxy.ClassMutable {
		t.Error("helm should auto-classify index.yaml as mutable")
	}
	if c.Classify(remote, "chart-1.0.tgz") != proxy.ClassImmutable {
		t.Error("helm should auto-classify .tgz as immutable")
	}
}

func TestClassifier_DockerAutoClassifies(t *testing.T) {
	c := proxy.NewClassifier(&docker.Provider{})
	remote := models.Remote{Name: "test"}
	if c.Classify(remote, "library/nginx/manifests/latest") != proxy.ClassMutable {
		t.Error("docker should classify tag manifest as mutable")
	}
	if c.Classify(remote, "library/nginx/manifests/sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890") != proxy.ClassImmutable {
		t.Error("docker should classify digest manifest as immutable")
	}
	if c.Classify(remote, "library/nginx/blobs/sha256:abc") != proxy.ClassImmutable {
		t.Error("docker should classify blobs as immutable")
	}
}

func TestClassifier_RPMAutoClassifies(t *testing.T) {
	c := proxy.NewClassifier(&rpm.Provider{})
	remote := models.Remote{Name: "test"}
	if c.Classify(remote, "repodata/primary.xml.gz") != proxy.ClassMutable {
		t.Error("rpm should classify repodata as mutable")
	}
	if c.Classify(remote, "packages/foo-1.0.rpm") != proxy.ClassImmutable {
		t.Error("rpm should classify .rpm as immutable")
	}
}
