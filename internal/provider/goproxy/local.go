package goproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	modzip "golang.org/x/mod/zip"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

type versionFile struct {
	module  string
	version string
	ext     string
}

// parseVersionFile splits "<escaped module>/@v/<version>.<ext>" into its parts.
func parseVersionFile(path string) (versionFile, error) {
	escMod, file, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/@v/")
	if !ok || escMod == "" || strings.Contains(file, "/") {
		return versionFile{}, fmt.Errorf("path %q must be <module>/@v/<version>.{zip,mod,info}", path)
	}
	dot := strings.LastIndex(file, ".")
	if dot <= 0 {
		return versionFile{}, fmt.Errorf("path %q must end in .zip, .mod or .info", path)
	}
	ext := file[dot+1:]
	if ext != "zip" && ext != "mod" && ext != "info" {
		return versionFile{}, fmt.Errorf("path %q must end in .zip, .mod or .info", path)
	}
	mod, err := module.UnescapePath(escMod)
	if err != nil {
		return versionFile{}, fmt.Errorf("module path must be case-encoded (uppercase as !lower): %w", err)
	}
	ver, err := module.UnescapeVersion(file[:dot])
	if err != nil {
		return versionFile{}, err
	}
	if err := module.Check(mod, ver); err != nil {
		return versionFile{}, err
	}
	if semver.Canonical(ver) != ver {
		return versionFile{}, fmt.Errorf("version %q is not canonical semver", ver)
	}
	return versionFile{module: mod, version: ver, ext: ext}, nil
}

func (v versionFile) storagePath() string {
	escMod, _ := module.EscapePath(v.module)
	escVer, _ := module.EscapeVersion(v.version)
	return escMod + "/@v/" + escVer + "." + v.ext
}

func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	vf, err := parseVersionFile(filePath)
	if err != nil {
		return "", "", err
	}
	sp := vf.storagePath()
	return sp, p.ContentType(sp), nil
}

func (p *Provider) ValidateContent(storagePath, localFile string) error {
	vf, err := parseVersionFile(storagePath)
	if err != nil {
		return err
	}
	switch vf.ext {
	case "zip":
		if _, err := modzip.CheckZip(module.Version{Path: vf.module, Version: vf.version}, localFile); err != nil {
			return fmt.Errorf("invalid module zip (files must sit under %s@%s/): %w", vf.module, vf.version, err)
		}
	case "mod":
		data, err := os.ReadFile(localFile)
		if err != nil {
			return err
		}
		mf, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			return fmt.Errorf("invalid go.mod: %w", err)
		}
		if mf.Module == nil || mf.Module.Mod.Path != vf.module {
			return fmt.Errorf("go.mod must declare module %s", vf.module)
		}
	case "info":
		data, err := os.ReadFile(localFile)
		if err != nil {
			return err
		}
		var info struct{ Version string }
		if err := json.Unmarshal(data, &info); err != nil {
			return fmt.Errorf("invalid .info JSON: %w", err)
		}
		if info.Version != vf.version {
			return fmt.Errorf(".info Version %q does not match %s", info.Version, vf.version)
		}
	}
	return nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	vf, _ := parseVersionFile(storagePath)
	return map[string]any{
		"module":       vf.module,
		"version":      vf.version,
		"path":         storagePath,
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

// versions returns the canonical versions of escMod that have a .mod uploaded, sorted ascending.
func versions(ctx context.Context, files provider.FileStore, repoName, escMod string) (versions []string, infos map[string]bool, err error) {
	entries, err := files.ListFilesByPrefix(ctx, repoName, escMod+"/@v/")
	if err != nil {
		return nil, nil, err
	}
	infos = map[string]bool{}
	for _, e := range entries {
		vf, err := parseVersionFile(e.FilePath)
		if err != nil || vf.storagePath() != e.FilePath {
			continue
		}
		switch vf.ext {
		case "mod":
			versions = append(versions, vf.version)
		case "info":
			infos[vf.version] = true
		}
	}
	semver.Sort(versions)
	return versions, infos, nil
}

// Latest is the highest release in sorted vs, else the highest prerelease.
func Latest(vs []string) string {
	for i := len(vs) - 1; i >= 0; i-- {
		if semver.Prerelease(vs[i]) == "" {
			return vs[i]
		}
	}
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

func infoJSON(version string) []byte {
	b, _ := json.Marshal(map[string]string{"Version": version})
	return b
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, path string) bool {
	path = strings.TrimPrefix(path, "/")
	switch {
	case strings.HasSuffix(path, "/@v/list"), strings.HasSuffix(path, "/@latest"):
	case strings.HasSuffix(path, ".info"):
		vf, err := parseVersionFile(path)
		if err != nil {
			return false
		}
		escMod, _ := module.EscapePath(vf.module)
		vs, infos, err := versions(r.Context(), files, repoName, escMod)
		if err != nil || infos[vf.version] || !slices.Contains(vs, vf.version) {
			return false
		}
	default:
		return false
	}

	body, err := p.GenerateLocalIndex(r.Context(), files, repoName, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return true
	}
	w.Header().Set("Content-Type", p.ContentType(path))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	path = strings.TrimPrefix(path, "/")
	if escMod, ok := strings.CutSuffix(path, "/@v/list"); ok {
		vs, _, err := versions(ctx, files, repoName, escMod)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for _, v := range vs {
			b.WriteString(v + "\n")
		}
		return []byte(b.String()), nil
	}
	if escMod, ok := strings.CutSuffix(path, "/@latest"); ok {
		vs, _, err := versions(ctx, files, repoName, escMod)
		if err != nil {
			return nil, err
		}
		v := Latest(vs)
		if v == "" {
			return nil, fmt.Errorf("no versions of %s", escMod)
		}
		return infoJSON(v), nil
	}
	if strings.HasSuffix(path, ".info") {
		vf, err := parseVersionFile(path)
		if err != nil {
			return nil, err
		}
		return infoJSON(vf.version), nil
	}
	return nil, fmt.Errorf("unsupported index path: %q", path)
}
