package virtual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	NPMAbbreviatedType = "application/vnd.npm.install-v1+json"
	npmAbbreviatedFile = "install-v1"
	maxNPMPackuments   = 1024
)

var npmVirtualNameRe = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~-]*$`)

// npmAbbreviatedFields are the version fields kept in an install-v1 packument.
var npmAbbreviatedFields = []string{
	"name", "version", "deprecated", "dependencies", "optionalDependencies",
	"devDependencies", "bundleDependencies", "peerDependencies", "peerDependenciesMeta",
	"bin", "directories", "dist", "engines", "_hasShrinkwrap", "hasInstallScript",
	"cpu", "os", "libc",
}

// NPMMember is one member's full packument for a package; Doc is nil when the
// member does not have the package. Tarball URLs under one of Bases are
// rewritten to go via the virtual; with no Bases (a local) they are relative.
type NPMMember struct {
	RemoteName string
	Bases      []string
	Doc        []byte
}

// NPMPackument is a merged packument in full and install-v1 form.
type NPMPackument struct {
	Full, Abbreviated []byte
}

func (p *NPMPackument) index() []byte { return p.Full }
func (p *NPMPackument) files() map[string][]byte {
	return map[string][]byte{npmAbbreviatedFile: p.Abbreviated}
}

// MergeNPM unions versions across members (first member wins a duplicate),
// keeps the first member's value for each dist-tag, drops tags naming versions
// outside the merged set and points every owned tarball at
// tarballBase + <member>/<path>. ErrNotFound means no member has the package.
func MergeNPM(members []NPMMember, tarballBase string) (*NPMPackument, error) {
	var base map[string]json.RawMessage
	versions := map[string]map[string]json.RawMessage{}
	tags := map[string]string{}
	times := map[string]json.RawMessage{}
	var modified string
	for _, m := range members {
		if m.Doc == nil {
			continue
		}
		var doc map[string]json.RawMessage
		var vs map[string]map[string]json.RawMessage
		var dt map[string]string
		var tm map[string]json.RawMessage
		if err := json.Unmarshal(m.Doc, &doc); err != nil {
			return nil, fmt.Errorf("member %q packument: %w", m.RemoteName, err)
		}
		for field, dst := range map[string]any{"versions": &vs, "dist-tags": &dt, "time": &tm} {
			if raw, ok := doc[field]; ok {
				if err := json.Unmarshal(raw, dst); err != nil {
					return nil, fmt.Errorf("member %q %s: %w", m.RemoteName, field, err)
				}
			}
		}
		if base == nil {
			base = doc
		}
		for v, man := range vs {
			if _, dup := versions[v]; dup || man == nil {
				continue
			}
			if err := rewriteNPMTarball(man, m, tarballBase); err != nil {
				return nil, fmt.Errorf("member %q %s: %w", m.RemoteName, v, err)
			}
			versions[v] = man
		}
		for tag, v := range dt {
			if _, ok := tags[tag]; !ok {
				tags[tag] = v
			}
		}
		for k, v := range tm {
			if k == "modified" {
				var s string
				if json.Unmarshal(v, &s) == nil && s > modified {
					modified = s
				}
				continue
			}
			if _, ok := times[k]; !ok {
				times[k] = v
			}
		}
	}
	if base == nil {
		return nil, ErrNotFound
	}
	for tag, v := range tags {
		if _, ok := versions[v]; !ok {
			delete(tags, tag)
		}
	}
	if modified != "" {
		times["modified"], _ = json.Marshal(modified)
	}

	full := map[string]any{}
	for k, v := range base {
		full[k] = v
	}
	full["versions"], full["dist-tags"] = versions, tags
	delete(full, "time")
	if len(times) > 0 {
		full["time"] = times
	}

	abbrevVersions := make(map[string]map[string]json.RawMessage, len(versions))
	for v, man := range versions {
		a := map[string]json.RawMessage{}
		for _, f := range npmAbbreviatedFields {
			if raw, ok := man[f]; ok {
				a[f] = raw
			}
		}
		abbrevVersions[v] = a
	}
	abbrev := map[string]any{"name": base["name"], "dist-tags": tags, "versions": abbrevVersions}
	if modified != "" {
		abbrev["modified"] = modified
	}

	fullJSON, err := json.Marshal(full)
	if err != nil {
		return nil, err
	}
	abbrevJSON, err := json.Marshal(abbrev)
	if err != nil {
		return nil, err
	}
	return &NPMPackument{Full: fullJSON, Abbreviated: abbrevJSON}, nil
}

// rewriteNPMTarball leaves a tarball outside the member's bases untouched.
func rewriteNPMTarball(man map[string]json.RawMessage, m NPMMember, tarballBase string) error {
	raw, ok := man["dist"]
	if !ok {
		return nil
	}
	var dist map[string]json.RawMessage
	if err := json.Unmarshal(raw, &dist); err != nil {
		return fmt.Errorf("dist: %w", err)
	}
	var tarball string
	if err := json.Unmarshal(dist["tarball"], &tarball); err != nil {
		return nil
	}
	rel, ok := npmMemberPath(tarball, m.Bases)
	if !ok {
		return nil
	}
	dist["tarball"], _ = json.Marshal(tarballBase + url.PathEscape(m.RemoteName) + "/" + rel)
	man["dist"], _ = json.Marshal(dist)
	return nil
}

func npmMemberPath(tarball string, bases []string) (string, bool) {
	if len(bases) == 0 {
		return tarball, tarball != "" && !strings.Contains(tarball, "://") && !strings.HasPrefix(tarball, "/")
	}
	for _, b := range bases {
		if rel, ok := strings.CutPrefix(tarball, strings.TrimRight(b, "/")+"/"); ok && rel != "" {
			return rel, true
		}
	}
	return "", false
}

// FetchNPM serves the merged packument for the package named by path, full or
// abbreviated (install-v1).
func (e *Engine) FetchNPM(ctx context.Context, virt models.Virtual, path, proxyBaseURL string, abbreviated bool) ([]byte, string, error) {
	pkg, err := url.PathUnescape(strings.TrimSuffix(path, "/"))
	if err != nil || !npmVirtualNameRe.MatchString(pkg) {
		return nil, "", ErrNotFound
	}
	prefix := "npm\x00" + virt.Name + "\x00"
	tarballBase := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/virtual/" + url.PathEscape(virt.Name) + "/"
	inserted := false
	repo, err := e.merged(ctx, prefix+pkg+"\x00"+proxyBaseURL, func(ctx context.Context) (mergedRepo, error) {
		inserted = true
		members, err := fetchMembers(virt, func(name string) (*NPMMember, error) { return e.npmMember(ctx, name, pkg) })
		if err != nil {
			return nil, err
		}
		p, err := MergeNPM(members, tarballBase)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
	if err != nil {
		return nil, "", err
	}
	if inserted {
		e.evictOldest(prefix, maxNPMPackuments)
	}
	if abbreviated {
		return repo.files()[npmAbbreviatedFile], NPMAbbreviatedType, nil
	}
	return repo.index(), "application/json", nil
}

// fetchNPMMember returns a member with a nil Doc when it lacks the package.
func (e *Engine) fetchNPMMember(ctx context.Context, name, pkg string) (*NPMMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	m := &NPMMember{RemoteName: name}
	if remote.RepoType == models.RepoTypeLocal {
		if e.NPMLocal == nil {
			return nil, errors.New("local npm packuments are not wired")
		}
		m.Doc, err = e.NPMLocal(ctx, name, pkg)
	} else {
		m.Bases = remote.UpstreamPool()
		m.Doc, err = e.fetchMemberPath(ctx, *remote, strings.Replace(pkg, "/", "%2f", 1))
	}
	var pe *proxy.ProxyError
	if errors.Is(err, ErrNotFound) || errors.As(err, &pe) && pe.Status == http.StatusNotFound {
		return &NPMMember{RemoteName: name}, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// evictOldest drops the oldest merges under prefix beyond keep.
func (e *Engine) evictOldest(prefix string, keep int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var keys []string
	for k := range e.gens {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	if len(keys) <= keep {
		return
	}
	slices.SortFunc(keys, func(a, b string) int { return e.gens[b].at.Compare(e.gens[a].at) })
	for _, k := range keys[keep:] {
		delete(e.gens, k)
	}
}
