package virtual

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// debIndexRe matches the binary Packages indexes a virtual merges: the flat
// repo's root Packages and a suite's <component>/binary-<arch>/Packages.
var debIndexRe = regexp.MustCompile(`^([^/]+/binary-[^/]+/)?Packages(\.[a-z0-9]+)?$`)

// debByHashRe matches an index requested by its SHA256 (Acquire-By-Hash).
var debByHashRe = regexp.MustCompile(`^([^/]+/binary-[^/]+/)?by-hash/SHA256/[0-9a-f]{64}$`)

// debCompressions is the order a member's index variants are fetched in.
var debCompressions = []string{".xz", ".gz", ".zst", ".bz2", ""}

// DebMember is one member's Release plus its decompressed Packages indexes,
// keyed by index directory relative to the Release ("" for a flat repo).
type DebMember struct {
	RemoteName string
	Release    []byte
	Packages   map[string][]byte
}

// DebRepo is a merged apt repo: Release plus the indexes it lists, keyed by
// their path relative to the Release.
type DebRepo struct {
	Release []byte
	Files   map[string][]byte
}

type debRelease struct {
	fields map[string]string
	files  []string
}

// parseDebRelease reads a Release file's single-line fields and the names
// listed in its checksum sections.
func parseDebRelease(body []byte) debRelease {
	rel := debRelease{fields: map[string]string{}}
	inSums := false
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if f := strings.Fields(line); inSums && len(f) == 3 && !seen[f[2]] {
				seen[f[2]] = true
				rel.files = append(rel.files, f[2])
			}
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		inSums = v == "" && (k == "MD5Sum" || strings.HasPrefix(k, "SHA"))
		if _, ok := rel.fields[k]; !ok {
			rel.fields[k] = v
		}
	}
	return rel
}

// debIndexFiles returns, per index directory, the variant of its Packages file
// to fetch from a member's Release listing.
func debIndexFiles(rel debRelease) map[string]string {
	listed := map[string]bool{}
	for _, f := range rel.files {
		listed[f] = true
	}
	out := map[string]string{}
	for _, f := range rel.files {
		m := debIndexRe.FindStringSubmatch(f)
		if m == nil || out[m[1]] != "" {
			continue
		}
		for _, c := range debCompressions {
			if listed[m[1]+"Packages"+c] {
				out[m[1]] = m[1] + "Packages" + c
				break
			}
		}
	}
	return out
}

func splitStanzas(body []byte) []string {
	var out []string
	for s := range strings.SplitSeq(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
		if s = strings.Trim(s, "\n"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stanzaField(stanza, key string) string {
	for line := range strings.SplitSeq(stanza, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func unionFields(dst []string, field string) []string {
	for f := range strings.FieldsSeq(field) {
		if !slices.Contains(dst, f) {
			dst = append(dst, f)
		}
	}
	return dst
}

func parseDebDate(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC1123Z, time.RFC1123} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// MergeDeb merges members into one apt repo. Members are in priority order: the
// first member to carry a (Package, Version, Architecture) in an index wins;
// stanzas missing any of those are always kept. Filename is prefixed with the
// owning member's name so the virtual can route downloads back to it.
func MergeDeb(members []DebMember) *DebRepo {
	var suite, codename string
	var arches, comps, dirs []string
	date := time.Unix(0, 0)
	for _, m := range members {
		rel := parseDebRelease(m.Release)
		if suite == "" {
			suite, codename = rel.fields["Suite"], rel.fields["Codename"]
		}
		arches = unionFields(arches, rel.fields["Architectures"])
		comps = unionFields(comps, rel.fields["Components"])
		if t, ok := parseDebDate(rel.fields["Date"]); ok && t.After(date) {
			date = t
		}
		for d := range m.Packages {
			if !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}
	slices.Sort(dirs)

	repo := &DebRepo{Files: map[string][]byte{}}
	for _, d := range dirs {
		seen := map[string]bool{}
		var b bytes.Buffer
		for _, m := range members {
			for _, s := range splitStanzas(m.Packages[d]) {
				pkg, ver, arch := stanzaField(s, "Package"), stanzaField(s, "Version"), stanzaField(s, "Architecture")
				if pkg != "" && ver != "" && arch != "" {
					key := pkg + "\x00" + ver + "\x00" + arch
					if seen[key] {
						continue
					}
					seen[key] = true
				}
				if fn := stanzaField(s, "Filename"); fn != "" {
					s = rewriteFilename(s, m.RemoteName+"/"+strings.TrimPrefix(path.Clean("/"+fn), "/"))
				}
				b.WriteString(s)
				b.WriteString("\n\n")
			}
		}
		gz := gzipDeterministic(b.Bytes())
		repo.Files[d+"Packages"] = b.Bytes()
		repo.Files[d+"Packages.gz"] = gz
		repo.Files[d+"by-hash/SHA256/"+sha256Hex(b.Bytes())] = b.Bytes()
		repo.Files[d+"by-hash/SHA256/"+sha256Hex(gz)] = gz
	}

	var r bytes.Buffer
	if suite != "" {
		fmt.Fprintf(&r, "Suite: %s\n", suite)
	}
	if codename != "" {
		fmt.Fprintf(&r, "Codename: %s\n", codename)
	}
	fmt.Fprintf(&r, "Date: %s\n", date.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&r, "Architectures: %s\n", strings.Join(arches, " "))
	if len(comps) > 0 {
		fmt.Fprintf(&r, "Components: %s\n", strings.Join(comps, " "))
	}
	r.WriteString("Acquire-By-Hash: yes\n")
	for _, sec := range []struct {
		name string
		sum  func([]byte) string
	}{{"MD5Sum", md5Hex}, {"SHA256", sha256Hex}} {
		r.WriteString(sec.name + ":\n")
		for _, d := range dirs {
			for _, n := range []string{d + "Packages", d + "Packages.gz"} {
				fmt.Fprintf(&r, " %s %d %s\n", sec.sum(repo.Files[n]), len(repo.Files[n]), n)
			}
		}
	}
	repo.Release = r.Bytes()
	return repo
}

func rewriteFilename(stanza, fn string) string {
	lines := strings.Split(stanza, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "Filename:") {
			lines[i] = "Filename: " + fn
			break
		}
	}
	return strings.Join(lines, "\n")
}

// debIndexPath splits an index request into its Release directory and the file
// under it. ok is false for anything a deb virtual does not merge.
func debIndexPath(p string) (base, name string, ok bool) {
	if rest, found := strings.CutPrefix(p, "dists/"); found {
		suite, n, found := strings.Cut(rest, "/")
		if !found || suite == "" {
			return "", "", false
		}
		base, p = "dists/"+suite+"/", n
	}
	if p != "Release" && !debIndexRe.MatchString(p) && !debByHashRe.MatchString(p) {
		return "", "", false
	}
	return base, p, true
}

func md5Hex(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}
