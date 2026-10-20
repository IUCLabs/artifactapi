package virtual

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/provider/puppet"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	maxPuppetModules     = 1024
	puppetMemberPage     = 100
	maxPuppetMemberPages = 100
)

// PuppetMember is one member's releases of a module; Found is false when the
// member does not have it.
type PuppetMember struct {
	RemoteName string
	Found      bool
	Releases   []map[string]any
}

// PuppetDoc is a module's merged releases, newest version first, with
// file_uri routed via the virtual; Owners maps each version to its member;
// missing caches that no member has the module.
type PuppetDoc struct {
	Releases []map[string]any
	Owners   map[string]string
	body     []byte
	missing  bool
}

func (d *PuppetDoc) index() []byte            { return d.body }
func (d *PuppetDoc) files() map[string][]byte { return nil }

// MergePuppetReleases merges member releases of slug; the earlier member wins
// a duplicate version. A release whose file is not <slug>-<version>.tar.gz is
// dropped, since its download could not be routed. ErrNotFound means no
// member has the module.
func MergePuppetReleases(slug string, members []PuppetMember) (*PuppetDoc, error) {
	d := &PuppetDoc{Releases: []map[string]any{}, Owners: map[string]string{}}
	found := false
	for _, m := range members {
		found = found || m.Found
		for _, r := range m.Releases {
			v, _ := r["version"].(string)
			file, _ := r["file_uri"].(string)
			name := path.Base(file)
			s, fv, ok := puppet.ParseRelease(strings.TrimSuffix(name, ".tar.gz"))
			if !ok || s != slug || fv != v || !strings.HasSuffix(name, ".tar.gz") {
				continue
			}
			if _, dup := d.Owners[v]; dup {
				continue
			}
			d.Owners[v] = m.RemoteName
			r["file_uri"] = "/v3/files/" + name
			d.Releases = append(d.Releases, r)
		}
	}
	if !found {
		return nil, ErrNotFound
	}
	slices.SortStableFunc(d.Releases, func(a, b map[string]any) int {
		return puppet.CompareSemver(b["version"].(string), a["version"].(string))
	})
	var err error
	d.body, err = json.Marshal(d.Releases)
	return d, err
}

// PuppetModule is the /v3/modules/<slug> document for d: the newest release
// as current_release and every release listed.
func PuppetModule(d *PuppetDoc) (map[string]any, error) {
	if len(d.Releases) == 0 {
		return nil, ErrNotFound
	}
	top := d.Releases[0]
	ref, ok := top["module"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("release %v has no module", top["slug"])
	}
	mod := maps.Clone(ref)
	releases := make([]map[string]any, len(d.Releases))
	for i, r := range d.Releases {
		releases[i] = map[string]any{}
		for _, k := range []string{"uri", "slug", "version", "file_uri", "file_size", "supported", "created_at", "deleted_at"} {
			releases[i][k] = r[k]
		}
	}
	mod["current_release"] = top
	mod["releases"] = releases
	return mod, nil
}

// PuppetReleases returns the merged releases of slug across members.
func (e *Engine) PuppetReleases(ctx context.Context, virt models.Virtual, slug string) (*PuppetDoc, error) {
	prefix := "puppet\x00" + virt.Name + "\x00"
	inserted := false
	repo, err := e.merged(ctx, prefix+slug, func(ctx context.Context) (mergedRepo, error) {
		inserted = true
		members, err := fetchMembers(virt, func(m string) (*PuppetMember, error) { return e.puppetMember(ctx, m, slug) })
		if err != nil {
			return nil, err
		}
		d, err := MergePuppetReleases(slug, members)
		if errors.Is(err, ErrNotFound) {
			return &PuppetDoc{missing: true}, nil
		}
		return d, err
	})
	if err != nil {
		return nil, err
	}
	if inserted {
		e.evictOldest(prefix, maxPuppetModules)
	}
	if d := repo.(*PuppetDoc); !d.missing {
		return d, nil
	}
	return nil, ErrNotFound
}

// PuppetRedirect maps v3/releases/<slug>-<version> or v3/files/<file> to the
// route of the member owning that release.
func (e *Engine) PuppetRedirect(ctx context.Context, virt models.Virtual, p, proxyBaseURL string) (string, error) {
	rel, ok := strings.CutPrefix(p, "v3/releases/")
	if !ok {
		file, isFile := strings.CutPrefix(p, "v3/files/")
		if rel, ok = strings.CutSuffix(file, ".tar.gz"); !isFile || !ok {
			return "", ErrNotFound
		}
	}
	slug, version, ok := puppet.ParseRelease(rel)
	if !ok {
		return "", ErrNotFound
	}
	d, err := e.PuppetReleases(ctx, virt, slug)
	if err != nil {
		return "", err
	}
	owner, ok := d.Owners[version]
	if !ok {
		return "", ErrNotFound
	}
	loc, ok, err := e.memberLocation(ctx, owner, p, "", proxyBaseURL)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("member %q not found", owner)
	}
	return loc, nil
}

// puppetMember reads every release a member lists for slug, following its
// pagination.
func (e *Engine) puppetMember(ctx context.Context, member, slug string) (*PuppetMember, error) {
	remote, err := e.getRemote(ctx, member)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", member, err)
	}
	m := &PuppetMember{RemoteName: member}
	q := url.Values{"module": {slug}, "limit": {strconv.Itoa(puppetMemberPage)}, "exclude_fields": {"readme changelog license reference"}}
	for page := 0; page < maxPuppetMemberPages; page++ {
		if page > 0 {
			q.Set("offset", strconv.Itoa(len(m.Releases)))
		}
		p := "v3/releases?" + q.Encode()
		body, err := e.puppetDoc(ctx, *remote, p)
		var pe *proxy.ProxyError
		if errors.Is(err, ErrNotFound) || errors.As(err, &pe) && pe.Status == http.StatusNotFound {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Pagination struct{ Total int }
			Results    []map[string]any
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("%s/%s: %v", member, p, err)
		}
		m.Found = m.Found || doc.Pagination.Total > 0
		m.Releases = append(m.Releases, doc.Results...)
		if len(m.Releases) >= doc.Pagination.Total {
			return m, nil
		}
		if len(doc.Results) == 0 {
			return nil, fmt.Errorf("%s/%s: empty page after %d of %d releases", member, p, len(m.Releases), doc.Pagination.Total)
		}
	}
	return nil, fmt.Errorf("%s: more than %d pages of %s releases", member, maxPuppetMemberPages, slug)
}
