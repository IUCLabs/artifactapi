package gems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	NameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	VersionRe  = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z]+)*(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	PlatformRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	opRe       = regexp.MustCompile(`^(=|!=|>|<|>=|<=|~>)$`)
)

const maxMetadataBytes = 8 << 20

type Requirement [][2]string

type Dependency struct {
	Name        string
	Requirement Requirement
	Type        string
	Prerelease  bool
}

// Spec is the subset of a gemspec the compact index and quick spec need.
type Spec struct {
	Name, Version, Platform string
	Summary, RubyGemsVer    string
	SpecVersion             int
	Date                    time.Time
	Authors, Licenses       []string
	Email, Description      string
	Homepage                string
	Metadata                map[string]string
	Dependencies            []Dependency
	RequiredRuby            Requirement
	RequiredRubyGems        Requirement
}

// FullName is the gem's file stem: name-version, plus -platform when not ruby.
func (s *Spec) FullName() string {
	if s.Platform == "" || s.Platform == "ruby" {
		return s.Name + "-" + s.Version
	}
	return s.Name + "-" + s.Version + "-" + s.Platform
}

type yamlReq struct {
	Requirements [][]yaml.Node `yaml:"requirements"`
}

func (r *yamlReq) decode() (Requirement, error) {
	if r == nil {
		return nil, nil
	}
	var out Requirement
	for _, pair := range r.Requirements {
		var op string
		var ver struct {
			Version string `yaml:"version"`
		}
		if len(pair) != 2 || pair[0].Decode(&op) != nil || pair[1].Decode(&ver) != nil {
			return nil, errors.New("malformed requirement")
		}
		if !opRe.MatchString(op) || !VersionRe.MatchString(ver.Version) {
			return nil, fmt.Errorf("invalid requirement %q %q", op, ver.Version)
		}
		out = append(out, [2]string{op, ver.Version})
	}
	return out, nil
}

// String renders the requirement as the compact index does: "op ver&op ver".
func (r Requirement) String() string {
	parts := make([]string, len(r))
	for i, p := range r {
		parts[i] = p[0] + " " + p[1]
	}
	return strings.Join(parts, "&")
}

func (r Requirement) trivial() bool {
	return len(r) == 0 || len(r) == 1 && r[0] == [2]string{">=", "0"}
}

// ParseGem reads the gemspec from a .gem archive (a tar holding metadata.gz).
func ParseGem(gem []byte) (*Spec, error) {
	tr := tar.NewReader(bytes.NewReader(gem))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("gem has no metadata.gz")
		}
		if err != nil {
			return nil, fmt.Errorf("read gem: %w", err)
		}
		if hdr.Name != "metadata.gz" {
			continue
		}
		zr, err := gzip.NewReader(tr)
		if err != nil {
			return nil, fmt.Errorf("metadata.gz: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(zr, maxMetadataBytes+1))
		if err != nil {
			return nil, fmt.Errorf("metadata.gz: %w", err)
		}
		if len(raw) > maxMetadataBytes {
			return nil, fmt.Errorf("metadata exceeds %d bytes", maxMetadataBytes)
		}
		return parseMetadata(raw)
	}
}

func parseMetadata(raw []byte) (*Spec, error) {
	var doc struct {
		Name    string `yaml:"name"`
		Version struct {
			Version string `yaml:"version"`
		} `yaml:"version"`
		Platform     string            `yaml:"platform"`
		Summary      string            `yaml:"summary"`
		Authors      []string          `yaml:"authors"`
		Email        yaml.Node         `yaml:"email"`
		Description  string            `yaml:"description"`
		Homepage     string            `yaml:"homepage"`
		Licenses     []string          `yaml:"licenses"`
		Metadata     map[string]string `yaml:"metadata"`
		Date         string            `yaml:"date"`
		RubyGemsVer  string            `yaml:"rubygems_version"`
		SpecVersion  int               `yaml:"specification_version"`
		Dependencies []struct {
			Name        string   `yaml:"name"`
			Requirement *yamlReq `yaml:"requirement"`
			VersionReqs *yamlReq `yaml:"version_requirements"`
			Type        string   `yaml:"type"`
			Prerelease  bool     `yaml:"prerelease"`
		} `yaml:"dependencies"`
		RequiredRuby     *yamlReq `yaml:"required_ruby_version"`
		RequiredRubyGems *yamlReq `yaml:"required_rubygems_version"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gemspec yaml: %w", err)
	}
	s := &Spec{
		Name: doc.Name, Version: doc.Version.Version, Platform: doc.Platform,
		Summary: doc.Summary, Authors: doc.Authors, Description: doc.Description,
		Homepage: doc.Homepage, Licenses: doc.Licenses, Metadata: doc.Metadata,
		RubyGemsVer: doc.RubyGemsVer, SpecVersion: doc.SpecVersion,
	}
	if s.Platform == "" {
		s.Platform = "ruby"
	}
	if !NameRe.MatchString(s.Name) || !VersionRe.MatchString(s.Version) || !PlatformRe.MatchString(s.Platform) {
		return nil, fmt.Errorf("invalid gem name/version/platform %q %q %q", s.Name, s.Version, s.Platform)
	}
	if doc.Email.Kind == yaml.ScalarNode {
		s.Email = doc.Email.Value
	}
	if t, err := time.Parse("2006-01-02", strings.SplitN(doc.Date, " ", 2)[0]); err == nil {
		s.Date = t
	}
	var err error
	if s.RequiredRuby, err = doc.RequiredRuby.decode(); err != nil {
		return nil, err
	}
	if s.RequiredRubyGems, err = doc.RequiredRubyGems.decode(); err != nil {
		return nil, err
	}
	for _, d := range doc.Dependencies {
		if !NameRe.MatchString(d.Name) {
			return nil, fmt.Errorf("invalid dependency name %q", d.Name)
		}
		req := d.Requirement
		if req == nil {
			req = d.VersionReqs
		}
		r, err := req.decode()
		if err != nil {
			return nil, fmt.Errorf("dependency %s: %w", d.Name, err)
		}
		typ := strings.TrimPrefix(d.Type, ":")
		if typ == "" {
			typ = "runtime"
		}
		s.Dependencies = append(s.Dependencies, Dependency{Name: d.Name, Requirement: r, Type: typ, Prerelease: d.Prerelease})
	}
	return s, nil
}

// InfoLine renders the spec's compact index /info line; checksum is the
// .gem's sha256 hex.
func InfoLine(s *Spec, gem []byte) string {
	sum := sha256.Sum256(gem)
	var deps []string
	for _, d := range s.Dependencies {
		if d.Type == "runtime" {
			r := d.Requirement
			if len(r) == 0 {
				r = Requirement{{">=", "0"}}
			}
			deps = append(deps, d.Name+":"+r.String())
		}
	}
	key := s.Version
	if s.Platform != "ruby" {
		key += "-" + s.Platform
	}
	line := key + " " + strings.Join(deps, ",") + "|checksum:" + hex.EncodeToString(sum[:])
	if !s.RequiredRuby.trivial() {
		line += ",ruby:" + s.RequiredRuby.String()
	}
	if !s.RequiredRubyGems.trivial() {
		line += ",rubygems:" + s.RequiredRubyGems.String()
	}
	return line
}

// InfoBody renders an /info document from its lines.
func InfoBody(lines []string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// InfoKey returns an /info line's version[-platform] token.
func InfoKey(line string) string {
	k, _, _ := strings.Cut(line, " ")
	return k
}

// InfoLines returns an /info body's version lines.
func InfoLines(body []byte) []string {
	var out []string
	for _, l := range strings.Split(string(body), "\n") {
		if l != "" && l != "---" {
			out = append(out, l)
		}
	}
	return out
}

func MD5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// VersionsLine renders one /versions entry from a gem's /info body.
func VersionsLine(name string, info []byte) string {
	var keys []string
	for _, l := range InfoLines(info) {
		keys = append(keys, InfoKey(l))
	}
	return name + " " + strings.Join(keys, ",") + " " + MD5Hex(info)
}

// VersionsHeader starts a /versions document.
func VersionsHeader(at time.Time) string {
	return "created_at: " + at.UTC().Format(time.RFC3339) + "\n---\n"
}

// QuickSpec returns the deflated Marshal.4.8 dump of the spec, as served at
// quick/Marshal.4.8/<full>.gemspec.rz.
func QuickSpec(s *Spec) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(MarshalSpec(s)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalSpec mirrors Gem::Specification#_dump wrapped by Marshal.dump.
func MarshalSpec(s *Spec) []byte {
	var in rbMarshal
	in.WriteString("\x04\x08")
	in.array(19)
	in.str(s.RubyGemsVer)
	in.WriteByte('i')
	in.fixnum(s.SpecVersion)
	in.str(s.Name)
	in.version(s.Version)
	in.time(s.Date)
	in.str(s.Summary)
	in.requirement(s.RequiredRuby)
	in.requirement(s.RequiredRubyGems)
	in.str(s.Platform)
	in.array(len(s.Dependencies))
	for _, d := range s.Dependencies {
		in.WriteByte('o')
		in.sym("Gem::Dependency")
		in.fixnum(4)
		in.sym("@name")
		in.str(d.Name)
		in.sym("@requirement")
		in.requirement(d.Requirement)
		in.sym("@type")
		in.sym(d.Type)
		in.sym("@prerelease")
		in.boolean(d.Prerelease)
	}
	in.str("")
	in.optStr(s.Email)
	in.strs(s.Authors)
	in.optStr(s.Description)
	in.optStr(s.Homepage)
	in.boolean(true)
	in.str(s.Platform)
	in.strs(s.Licenses)
	in.WriteByte('{')
	in.fixnum(len(s.Metadata))
	for _, k := range slices.Sorted(maps.Keys(s.Metadata)) {
		in.str(k)
		in.str(s.Metadata[k])
	}

	var out rbMarshal
	out.WriteString("\x04\x08u")
	out.sym("Gem::Specification")
	out.fixnum(in.Len())
	out.Write(in.Bytes())
	return out.Bytes()
}

// rbMarshal writes the Ruby Marshal 4.8 subset gemspecs use. It never emits
// object or symbol links, so every symbol is written in full.
type rbMarshal struct{ bytes.Buffer }

func (m *rbMarshal) fixnum(n int) {
	switch {
	case n == 0:
		m.WriteByte(0)
	case n > 0 && n < 123:
		m.WriteByte(byte(n + 5))
	case n < 0 && n > -124:
		m.WriteByte(byte(n - 5))
	default:
		var b []byte
		for x := n; len(b) < 4; x >>= 8 {
			b = append(b, byte(x))
			if (n >= 0 && x>>8 == 0) || (n < 0 && x>>8 == -1) {
				break
			}
		}
		if n >= 0 {
			m.WriteByte(byte(len(b)))
		} else {
			m.WriteByte(byte(-len(b)))
		}
		m.Write(b)
	}
}

func (m *rbMarshal) raw(s string) {
	m.fixnum(len(s))
	m.WriteString(s)
}

func (m *rbMarshal) sym(s string) {
	m.WriteByte(':')
	m.raw(s)
}

// str writes a UTF-8 String (an ivar-wrapped string with E=true).
func (m *rbMarshal) str(s string) {
	m.WriteString("I\"")
	m.raw(s)
	m.fixnum(1)
	m.sym("E")
	m.WriteByte('T')
}

func (m *rbMarshal) optStr(s string) {
	if s == "" {
		m.WriteByte('0')
		return
	}
	m.str(s)
}

func (m *rbMarshal) strs(ss []string) {
	m.array(len(ss))
	for _, s := range ss {
		m.str(s)
	}
}

func (m *rbMarshal) array(n int) {
	m.WriteByte('[')
	m.fixnum(n)
}

func (m *rbMarshal) boolean(b bool) {
	if b {
		m.WriteByte('T')
	} else {
		m.WriteByte('F')
	}
}

func (m *rbMarshal) version(v string) {
	m.WriteByte('U')
	m.sym("Gem::Version")
	m.array(1)
	m.str(v)
}

func (m *rbMarshal) requirement(r Requirement) {
	if len(r) == 0 {
		r = Requirement{{">=", "0"}}
	}
	m.WriteByte('U')
	m.sym("Gem::Requirement")
	m.array(1)
	m.array(len(r))
	for _, p := range r {
		m.array(2)
		m.str(p[0])
		m.version(p[1])
	}
}

// time writes a UTC Time with day resolution, as Time#_dump lays it out.
func (m *rbMarshal) time(t time.Time) {
	t = t.UTC()
	// Time#_dump holds year-1900 in 16 bits.
	if t.IsZero() || t.Year() < 1900 || t.Year() > 1900+0xffff {
		t = time.Unix(0, 0).UTC()
	}
	p := uint32(1)<<31 | uint32(1)<<30 | uint32(t.Year()-1900)<<14 | uint32(t.Month()-1)<<10 | uint32(t.Day())<<5 | uint32(t.Hour())
	s := uint32(t.Minute())<<26 | uint32(t.Second())<<20
	m.WriteString("Iu")
	m.sym("Time")
	m.fixnum(8)
	for _, w := range []uint32{p, s} {
		m.Write([]byte{byte(w), byte(w >> 8), byte(w >> 16), byte(w >> 24)})
	}
	m.fixnum(1)
	m.sym("zone")
	m.WriteString("I\"")
	m.raw("UTC")
	m.fixnum(1)
	m.sym("E")
	m.WriteByte('F')
}
