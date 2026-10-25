package virtual

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/IUCLabs/artifactapi/internal/provider/goproxy"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const maxGoModules = 1024

// GoMember is one member's response for a module path; Body is nil when the
// member does not have it (404/410).
type GoMember struct {
	RemoteName string
	Body       []byte
}

// GoDoc is a goproxy virtual's merged @v/list for one module. Owners maps each
// version to the first member listing it; missing caches that no member has it.
type GoDoc struct {
	Body    []byte
	Owners  map[string]string
	Latest  string
	missing bool
}

func (d *GoDoc) index() []byte {
	if d.missing {
		return nil
	}
	b := append(slices.Clone(d.Body), 0)
	for _, v := range slices.Sorted(maps.Keys(d.Owners)) {
		b = append(b, v+"="+d.Owners[v]+"\n"...)
	}
	return b
}

func (d *GoDoc) files() map[string][]byte { return nil }

// MergeGoList unions member @v/list documents, sorted by semver; the first
// member listing a version owns it. ErrNotFound means no member has the module.
func MergeGoList(members []GoMember) (*GoDoc, error) {
	d := &GoDoc{Owners: map[string]string{}}
	found := false
	var vs []string
	for _, m := range members {
		if m.Body == nil {
			continue
		}
		found = true
		for line := range strings.Lines(string(m.Body)) {
			f := strings.Fields(line)
			if len(f) == 0 || !semver.IsValid(f[0]) {
				continue
			}
			if _, dup := d.Owners[f[0]]; dup {
				continue
			}
			d.Owners[f[0]] = m.RemoteName
			vs = append(vs, f[0])
		}
	}
	if !found {
		return nil, ErrNotFound
	}
	semver.Sort(vs)
	for _, v := range vs {
		d.Body = append(d.Body, v+"\n"...)
	}
	if d.Body == nil {
		d.Body = []byte{}
	}
	d.Latest = goproxy.Latest(vs)
	return d, nil
}

// GoProxy serves a goproxy virtual path: the merged <module>/@v/list body, or a
// root-relative redirect for @latest and @v/<version>.{info,mod,zip} to the
// first member, in member order, that has that version.
func (e *Engine) GoProxy(ctx context.Context, virt models.Virtual, path, rawQuery string) (body []byte, loc string, err error) {
	escMod, rest, ok := strings.Cut(path, "/@")
	if !ok {
		return nil, "", ErrNotFound
	}
	if _, err := module.UnescapePath(escMod); err != nil {
		return nil, "", ErrNotFound
	}
	if rest == "v/list" {
		d, err := e.goList(ctx, virt, escMod)
		if err != nil {
			return nil, "", err
		}
		return d.Body, "", nil
	}

	var target, owner string
	var d *GoDoc
	switch {
	case rest == "latest":
		if d, err = e.goList(ctx, virt, escMod); err != nil {
			return nil, "", err
		}
		if d.Latest == "" {
			target = path
			owner, err = e.goProbe(ctx, virt, target)
			break
		}
		escVer, _ := module.EscapeVersion(d.Latest)
		target, owner = escMod+"/@v/"+escVer+".info", d.Owners[d.Latest]
	case strings.HasPrefix(rest, "v/"):
		escVer, ext, ok := cutGoExt(rest[len("v/"):])
		if !ok {
			return nil, "", ErrNotFound
		}
		ver, verr := module.UnescapeVersion(escVer)
		if verr != nil {
			return nil, "", ErrNotFound
		}
		if d, err = e.goList(ctx, virt, escMod); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, "", err
		}
		if d != nil {
			owner = d.Owners[ver]
		}
		if owner == "" {
			owner, err = e.goProbe(ctx, virt, escMod+"/@v/"+escVer+".info")
		}
		target = escMod + "/@v/" + escVer + "." + ext
	default:
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	loc, ok, err = e.memberLocation(ctx, owner, target, "", "")
	if err != nil || !ok {
		return nil, "", errors.Join(err, ErrNotFound)
	}
	// module paths and versions are validated, so the only escape is the case-encoding '!'
	loc = strings.ReplaceAll(loc, "%21", "!")
	if rawQuery != "" {
		loc += "?" + rawQuery
	}
	return nil, loc, nil
}

func cutGoExt(file string) (escVer, ext string, ok bool) {
	dot := strings.LastIndex(file, ".")
	if dot <= 0 || strings.Contains(file, "/") {
		return "", "", false
	}
	switch ext = file[dot+1:]; ext {
	case "info", "mod", "zip":
		return file[:dot], ext, true
	}
	return "", "", false
}

func (e *Engine) goList(ctx context.Context, virt models.Virtual, escMod string) (*GoDoc, error) {
	prefix := "goproxy\x00" + virt.Name + "\x00"
	inserted := false
	repo, err := e.merged(ctx, prefix+escMod, func(ctx context.Context) (mergedRepo, error) {
		inserted = true
		members, err := fetchMembers(virt, func(name string) (*GoMember, error) { return e.goMember(ctx, name, escMod+"/@v/list") })
		if err != nil {
			return nil, err
		}
		d, err := MergeGoList(members)
		if errors.Is(err, ErrNotFound) {
			return &GoDoc{missing: true}, nil
		}
		return d, err
	})
	if err != nil {
		return nil, err
	}
	if inserted {
		e.evictOldest(prefix, maxGoModules)
	}
	if d := repo.(*GoDoc); !d.missing {
		return d, nil
	}
	return nil, ErrNotFound
}

// goProbe returns the first member, in member order, that serves path, else
// the first member quarantining it so the client sees that refusal.
func (e *Engine) goProbe(ctx context.Context, virt models.Virtual, path string) (string, error) {
	held := ""
	for _, name := range virt.Members {
		m, err := e.goMember(ctx, name, path)
		if proxy.Quarantined(err) {
			if held == "" {
				held = name
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("virtual %q: member %q: %v", virt.Name, name, err)
		}
		if m.Body != nil {
			return name, nil
		}
	}
	if held != "" {
		return held, nil
	}
	return "", ErrNotFound
}

func (e *Engine) fetchGoMember(ctx context.Context, name, path string) (*GoMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	body, err := e.fetchMemberPath(ctx, *remote, path)
	var pe *proxy.ProxyError
	if errors.Is(err, ErrNotFound) || errors.As(err, &pe) && (pe.Status == http.StatusNotFound || pe.Status == http.StatusGone) {
		return &GoMember{RemoteName: name}, nil
	}
	if err != nil {
		return nil, err
	}
	if body == nil {
		body = []byte{}
	}
	return &GoMember{RemoteName: name, Body: body}, nil
}
