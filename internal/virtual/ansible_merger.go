package virtual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/provider/ansible"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	maxAnsibleCollections = 1024
	ansibleMemberPage     = 100
	maxAnsibleMemberPages = 100
)

// AnsibleMember is one member's versions of a collection; Found is false when
// the member does not have it.
type AnsibleMember struct {
	RemoteName string
	Found      bool
	Refs       []ansible.VersionRef
}

// AnsibleDoc is a collection's merged versions; Owners maps each version to
// the member serving it.
type AnsibleDoc struct {
	Refs   []ansible.VersionRef
	Owners map[string]string
	body   []byte
}

func (d *AnsibleDoc) index() []byte            { return d.body }
func (d *AnsibleDoc) files() map[string][]byte { return nil }

// MergeAnsibleVersions concatenates member versions in member order; the
// first member wins a duplicate version. ErrNotFound means no member has the
// collection.
func MergeAnsibleVersions(members []AnsibleMember) (*AnsibleDoc, error) {
	d := &AnsibleDoc{Refs: []ansible.VersionRef{}, Owners: map[string]string{}}
	found := false
	for _, m := range members {
		found = found || m.Found
		for _, r := range m.Refs {
			if _, dup := d.Owners[r.Version]; dup {
				continue
			}
			d.Owners[r.Version] = m.RemoteName
			d.Refs = append(d.Refs, r)
		}
	}
	if !found {
		return nil, ErrNotFound
	}
	var err error
	d.body, err = json.Marshal(d.Refs)
	return d, err
}

// AnsibleVersions returns the merged versions of ns.name across members.
func (e *Engine) AnsibleVersions(ctx context.Context, virt models.Virtual, ns, name string) (*AnsibleDoc, error) {
	prefix := "ansible\x00" + virt.Name + "\x00"
	inserted := false
	repo, err := e.merged(ctx, prefix+ns+"/"+name, func(ctx context.Context) (mergedRepo, error) {
		inserted = true
		members, err := fetchMembers(virt, func(m string) (*AnsibleMember, error) { return e.ansibleMember(ctx, m, ns, name) })
		if err != nil {
			return nil, err
		}
		return MergeAnsibleVersions(members)
	})
	if err != nil {
		return nil, err
	}
	if inserted {
		e.evictOldest(prefix, maxAnsibleCollections)
	}
	return repo.(*AnsibleDoc), nil
}

// AnsibleVersion serves the owning member's version detail with href and
// download_url pointing at the virtual; the download 302s to the member.
// Member documents carry root-relative URLs, so anything else is rejected.
func (e *Engine) AnsibleVersion(ctx context.Context, virt models.Virtual, ns, name, version, proxyBaseURL string) ([]byte, error) {
	d, err := e.AnsibleVersions(ctx, virt, ns, name)
	if err != nil {
		return nil, err
	}
	owner, ok := d.Owners[version]
	if !ok {
		return nil, ErrNotFound
	}
	remote, err := e.getRemote(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", owner, err)
	}
	body, err := e.ansibleDoc(ctx, *remote, ansible.VersionPath(ns, name, version)[1:])
	if err != nil {
		return nil, fmt.Errorf("member %q: %v", owner, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("member %q version detail: %v", owner, err)
	}
	dl, _ := detail["download_url"].(string)
	u, err := url.Parse(dl)
	if err != nil {
		return nil, fmt.Errorf("member %q download_url %q: %v", owner, dl, err)
	}
	rest, ok := strings.CutPrefix(u.EscapedPath(), "/api/v1/"+string(remote.RepoType)+"/"+url.PathEscape(owner)+"/")
	if !ok || u.Scheme != "" || u.Host != "" {
		return nil, fmt.Errorf("member %q download_url %q is not served by the member", owner, dl)
	}
	base := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/virtual/" + url.PathEscape(virt.Name)
	detail["download_url"] = base + "/" + url.PathEscape(owner) + "/" + rest
	detail["href"] = base + ansible.VersionPath(ns, name, version)
	return json.Marshal(detail)
}

// ansibleMember reads every version a member lists for ns.name, following
// the member's pagination for remotes.
func (e *Engine) ansibleMember(ctx context.Context, member, ns, name string) (*AnsibleMember, error) {
	remote, err := e.getRemote(ctx, member)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", member, err)
	}
	m := &AnsibleMember{RemoteName: member}
	list := ansible.CollectionPath(ns, name)[1:] + "versions/"
	for page := 0; page < maxAnsibleMemberPages; page++ {
		// Locals list every version at once; remote pages match the keys the
		// remote caches client requests under.
		path := list
		if remote.RepoType != models.RepoTypeLocal {
			path += "?limit=" + strconv.Itoa(ansibleMemberPage)
			if page > 0 {
				path += "&offset=" + strconv.Itoa(len(m.Refs))
			}
		}
		body, err := e.ansibleDoc(ctx, *remote, path)
		var pe *proxy.ProxyError
		if errors.Is(err, ErrNotFound) || errors.As(err, &pe) && pe.Status == http.StatusNotFound {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Meta struct{ Count int }
			Data []ansible.VersionRef
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("%s/%s: %v", member, path, err)
		}
		m.Found = true
		m.Refs = append(m.Refs, doc.Data...)
		if remote.RepoType == models.RepoTypeLocal || len(m.Refs) >= doc.Meta.Count {
			return m, nil
		}
		if len(doc.Data) == 0 {
			return nil, fmt.Errorf("%s/%s: empty page after %d of %d versions", member, path, len(m.Refs), doc.Meta.Count)
		}
	}
	return nil, fmt.Errorf("%s: more than %d pages of %s.%s versions", member, maxAnsibleMemberPages, ns, name)
}

// ansibleMemberDoc reads a Galaxy API document from a member.
func (e *Engine) ansibleMemberDoc(ctx context.Context, remote models.Remote, path string) ([]byte, error) {
	if remote.RepoType != models.RepoTypeLocal {
		return e.fetchMemberPath(ctx, remote, path)
	}
	if e.AnsibleLocal == nil {
		return nil, errors.New("local ansible indexes are not wired")
	}
	return e.AnsibleLocal(ctx, remote.Name, path)
}
