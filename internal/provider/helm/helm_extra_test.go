package helm

import "testing"

func TestHelmContentTypeBranches(t *testing.T) {
	p := &Provider{}
	for path, want := range map[string]string{
		"charts/x-1.0.0.tgz": "application/gzip",
		"x.tar.gz":           "application/gzip",
		"index.yaml":         "text/yaml",
		"x.yml":              "text/yaml",
		"other":              "application/octet-stream",
	} {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q)=%q want %q", path, got, want)
		}
	}
}
