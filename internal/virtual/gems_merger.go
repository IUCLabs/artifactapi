package virtual

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider/gems"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	maxGemsInfos      = 1024
	maxGemsCandidates = 8
)

// GemsMember is one member's compact index document; Body is nil when the
// member does not have it.
type GemsMember struct {
	RemoteName string
	Body       []byte
}

// GemsDoc is a merged compact index document. Owners maps each /info
// version[-platform] to the member serving it.
type GemsDoc struct {
	Body   []byte
	Owners map[string]string
}

func (d *GemsDoc) index() []byte            { return d.Body }
func (d *GemsDoc) files() map[string][]byte { return nil }

// MergeGemsInfo unions /info lines across members; the first member wins a
// duplicate version-platform. A gem only one member has is passed through
// verbatim so its md5 matches that member's /versions. ErrNotFound means no
// member has the gem.
func MergeGemsInfo(members []GemsMember) (*GemsDoc, error) {
	var have []GemsMember
	for _, m := range members {
		if m.Body != nil {
			have = append(have, m)
		}
	}
	if len(have) == 0 {
		return nil, ErrNotFound
	}
	d := &GemsDoc{Owners: map[string]string{}}
	var lines []string
	for _, m := range have {
		for _, l := range gems.InfoLines(m.Body) {
			k := gems.InfoKey(l)
			if _, dup := d.Owners[k]; dup {
				continue
			}
			d.Owners[k] = m.RemoteName
			lines = append(lines, l)
		}
	}
	if len(have) == 1 {
		d.Body = have[0].Body
	} else {
		d.Body = gems.InfoBody(lines)
	}
	return d, nil
}

// versionsLines returns the entry lines of a /versions document.
func versionsLines(m GemsMember) ([]byte, error) {
	if m.Body == nil {
		return nil, fmt.Errorf("member %q has no versions", m.RemoteName)
	}
	if bytes.HasPrefix(m.Body, []byte("---\n")) {
		return m.Body[4:], nil
	}
	_, rest, ok := bytes.Cut(m.Body, []byte("\n---\n"))
	if !ok {
		return nil, fmt.Errorf("member %q versions has no --- separator", m.RemoteName)
	}
	return rest, nil
}

func gemName(line []byte) string {
	n, _, _ := bytes.Cut(line, []byte(" "))
	return string(bytes.TrimSpace(n))
}

// MergeGemsVersions passes through the lines of gems only one member lists
// and writes one regenerated line, from the merged /info, per gem that
// several members list.
func MergeGemsVersions(members []GemsMember, info func(name string) (*GemsDoc, error), at time.Time) ([]byte, error) {
	bodies := make([][]byte, len(members))
	owners := map[string]int{}
	for i, m := range members {
		rest, err := versionsLines(m)
		if err != nil {
			return nil, err
		}
		bodies[i] = rest
		seen := map[string]bool{}
		for line := range bytes.Lines(rest) {
			if n := gemName(line); n != "" && !seen[n] {
				seen[n] = true
				owners[n]++
			}
		}
	}
	var b bytes.Buffer
	b.WriteString(gems.VersionsHeader(at))
	var shared []string
	for _, rest := range bodies {
		for line := range bytes.Lines(rest) {
			n := gemName(line)
			if n == "" {
				continue
			}
			if owners[n] > 1 {
				if !slices.Contains(shared, n) {
					shared = append(shared, n)
				}
				continue
			}
			b.Write(bytes.TrimRight(line, "\n"))
			b.WriteByte('\n')
		}
	}
	for _, n := range shared {
		d, err := info(n)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		b.WriteString(gems.VersionsLine(n, d.Body) + "\n")
	}
	return b.Bytes(), nil
}

// MergeGemsNames unions member /names documents, sorted.
func MergeGemsNames(members []GemsMember) ([]byte, error) {
	set := map[string]bool{}
	for _, m := range members {
		rest, err := versionsLines(m)
		if err != nil {
			return nil, err
		}
		for line := range bytes.Lines(rest) {
			if n := gemName(line); n != "" {
				set[n] = true
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	return []byte("---\n" + strings.Join(names, "\n") + "\n"), nil
}

// FetchGems serves a gems virtual's merged versions, names or info/<gem>.
func (e *Engine) FetchGems(ctx context.Context, virt models.Virtual, path string) ([]byte, string, error) {
	var d *GemsDoc
	var err error
	switch {
	case strings.HasPrefix(path, "info/"):
		name := strings.TrimPrefix(path, "info/")
		if !gems.NameRe.MatchString(name) {
			return nil, "", ErrNotFound
		}
		d, err = e.gemsInfo(ctx, virt, name)
	case path == "versions":
		d, err = e.gemsMerged(ctx, virt, path, func(ctx context.Context, ms []GemsMember) ([]byte, error) {
			return MergeGemsVersions(ms, func(name string) (*GemsDoc, error) { return e.gemsInfo(ctx, virt, name) }, time.Now())
		})
	case path == "names":
		d, err = e.gemsMerged(ctx, virt, path, func(_ context.Context, ms []GemsMember) ([]byte, error) {
			return MergeGemsNames(ms)
		})
	default:
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return d.Body, "text/plain; charset=utf-8", nil
}

func (e *Engine) gemsMerged(ctx context.Context, virt models.Virtual, path string, merge func(context.Context, []GemsMember) ([]byte, error)) (*GemsDoc, error) {
	repo, err := e.merged(ctx, "gems\x00"+virt.Name+"\x00"+path, func(ctx context.Context) (mergedRepo, error) {
		members, err := fetchMembers(virt, func(name string) (*GemsMember, error) { return e.gemsMember(ctx, name, path) })
		if err != nil {
			return nil, err
		}
		body, err := merge(ctx, members)
		if err != nil {
			return nil, err
		}
		return &GemsDoc{Body: body}, nil
	})
	if err != nil {
		return nil, err
	}
	return repo.(*GemsDoc), nil
}

func (e *Engine) gemsInfo(ctx context.Context, virt models.Virtual, name string) (*GemsDoc, error) {
	prefix := "gems\x00" + virt.Name + "\x00info/"
	inserted := false
	repo, err := e.merged(ctx, prefix+name, func(ctx context.Context) (mergedRepo, error) {
		inserted = true
		members, err := fetchMembers(virt, func(m string) (*GemsMember, error) { return e.gemsMember(ctx, m, "info/"+name) })
		if err != nil {
			return nil, err
		}
		return MergeGemsInfo(members)
	})
	if err != nil {
		return nil, err
	}
	if inserted {
		e.evictOldest(prefix, maxGemsInfos)
	}
	return repo.(*GemsDoc), nil
}

// GemsRedirect maps a gems virtual download (gems/<full>.gem or
// quick/Marshal.4.8/<full>.gemspec.rz) to the member whose /info line won the
// merge for that version-platform.
func (e *Engine) GemsRedirect(ctx context.Context, virt models.Virtual, path, rawQuery, proxyBaseURL string) (string, error) {
	full, ok := strings.CutPrefix(path, "gems/")
	if ok {
		full, ok = strings.CutSuffix(full, ".gem")
	} else if full, ok = strings.CutPrefix(path, "quick/Marshal.4.8/"); ok {
		full, ok = strings.CutSuffix(full, ".gemspec.rz")
	}
	if !ok || strings.Contains(full, "/") {
		return "", ErrNotFound
	}
	tried := 0
	for i := 1; i < len(full)-1 && tried < maxGemsCandidates; i++ {
		if full[i] != '-' || full[i+1] < '0' || full[i+1] > '9' || !gems.NameRe.MatchString(full[:i]) {
			continue
		}
		tried++
		d, err := e.gemsInfo(ctx, virt, full[:i])
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if owner, ok := d.Owners[full[i+1:]]; ok {
			loc, ok, err := e.memberLocation(ctx, owner, path, rawQuery, proxyBaseURL)
			if err == nil && !ok {
				err = ErrNotFound
			}
			return loc, err
		}
	}
	return "", ErrNotFound
}

// fetchGemsMember returns a nil Body when the member lacks the document.
func (e *Engine) fetchGemsMember(ctx context.Context, name, path string) (*GemsMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	m := &GemsMember{RemoteName: name}
	if remote.RepoType == models.RepoTypeLocal {
		if e.GemsLocal == nil {
			return nil, errors.New("local gems indexes are not wired")
		}
		m.Body, err = e.GemsLocal(ctx, name, path)
	} else {
		m.Body, err = e.fetchMemberPath(ctx, *remote, path)
	}
	var pe *proxy.ProxyError
	if errors.Is(err, ErrNotFound) || errors.As(err, &pe) && pe.Status == http.StatusNotFound {
		return &GemsMember{RemoteName: name}, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}
