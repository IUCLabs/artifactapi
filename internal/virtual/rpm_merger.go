package virtual

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

var rpmDataTypes = []string{"primary", "filelists", "other"}

var rpmRoots = map[string]string{
	"primary":   `<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="%d">`,
	"filelists": `<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="%d">`,
	"other":     `<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="%d">`,
}

var rpmRootClose = map[string]string{"primary": "</metadata>", "filelists": "</filelists>", "other": "</otherdata>"}

var locationRe = regexp.MustCompile(`<location\b[^>]*/>`)

// RPMMember is one member repo's decompressed primary/filelists/other XML,
// keyed by repomd data type. Missing types contribute no packages.
type RPMMember struct {
	RemoteName string
	Bases      []string
	Timestamp  int64
	Data       map[string][]byte
}

// RPMRepo is a merged yum repo: repomd.xml plus the files it references, keyed
// by their path under the repo root.
type RPMRepo struct {
	Repomd []byte
	Files  map[string][]byte
}

type repomdDoc struct {
	Revision string       `xml:"revision"`
	Data     []repomdData `xml:"data"`
}

type repomdData struct {
	Type     string `xml:"type,attr"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Timestamp int64 `xml:"timestamp"`
}

type rpmPkgVersion struct {
	Epoch string `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
}

type primaryPkg struct {
	Name     string        `xml:"name"`
	Arch     string        `xml:"arch"`
	Version  rpmPkgVersion `xml:"version"`
	Checksum string        `xml:"checksum"`
	Location struct {
		Href string `xml:"href,attr"`
		Base string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	} `xml:"location"`
}

type pkgidPkg struct {
	PkgID string `xml:"pkgid,attr"`
}

// parseRepomd returns the member's primary/filelists/other locations and its
// newest data timestamp.
func parseRepomd(body []byte) (map[string]string, int64, error) {
	var doc repomdDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, 0, fmt.Errorf("parse repomd.xml: %w", err)
	}
	locs := map[string]string{}
	var ts int64
	for _, d := range doc.Data {
		for _, t := range rpmDataTypes {
			if d.Type == t && d.Location.Href != "" {
				locs[t] = d.Location.Href
				ts = max(ts, d.Timestamp)
			}
		}
	}
	if locs["primary"] == "" {
		return nil, 0, errors.New("repomd.xml has no primary data")
	}
	return locs, ts, nil
}

func decompress(href string, body []byte) ([]byte, error) {
	var r io.Reader
	switch {
	case strings.HasSuffix(href, ".gz"):
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r = gz
	case strings.HasSuffix(href, ".xz"):
		x, err := xz.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r = x
	case strings.HasSuffix(href, ".zst"):
		z, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer z.Close()
		r = z
	case strings.HasSuffix(href, ".bz2"):
		r = bzip2.NewReader(bytes.NewReader(body))
	default:
		return body, nil
	}
	return io.ReadAll(r)
}

// splitPackages returns the raw bytes of each top-level <package> element.
func splitPackages(doc []byte) ([][]byte, error) {
	d := xml.NewDecoder(bytes.NewReader(doc))
	var pkgs [][]byte
	depth := 0
	for {
		start := d.InputOffset()
		tok, err := d.Token()
		if err == io.EOF {
			return pkgs, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 1 && t.Name.Local == "package" {
				if err := d.Skip(); err != nil {
					return nil, err
				}
				pkgs = append(pkgs, doc[start:d.InputOffset()])
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
}

// memberHref returns href relative to the member root. An xml:base under one of
// the member's upstream bases is folded into the href; any other xml:base is a
// host the member can't serve, so ok is false and the location is left as is.
func memberHref(m RPMMember, base, href string) (string, bool) {
	href = strings.TrimLeft(href, "/")
	if base == "" {
		return href, true
	}
	base = strings.TrimRight(base, "/") + "/"
	for _, b := range m.Bases {
		b = strings.TrimRight(b, "/") + "/"
		if strings.HasPrefix(base, b) {
			return strings.TrimPrefix(base, b) + href, true
		}
	}
	return "", false
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// MergeRPM merges members into one repo. Members are in priority order: the
// first member to carry a NEVRA wins. Package locations are prefixed with the
// owning member's name so the virtual can route downloads back to it.
func MergeRPM(members []RPMMember) (*RPMRepo, error) {
	kept := map[string]bool{}
	seen := map[string]bool{}
	out := map[string][][]byte{}

	for i, m := range members {
		pkgs, err := splitPackages(m.Data["primary"])
		if err != nil {
			return nil, fmt.Errorf("member %q primary: %w", m.RemoteName, err)
		}
		for _, raw := range pkgs {
			var p primaryPkg
			if err := xml.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("member %q primary package: %w", m.RemoteName, err)
			}
			epoch := p.Version.Epoch
			if epoch == "" {
				epoch = "0"
			}
			nevra := fmt.Sprintf("%s-%s:%s-%s.%s", p.Name, epoch, p.Version.Ver, p.Version.Rel, p.Arch)
			if seen[nevra] {
				continue
			}
			seen[nevra] = true
			kept[fmt.Sprintf("%d/%s", i, p.Checksum)] = true

			if href, ok := memberHref(m, p.Location.Base, p.Location.Href); ok {
				loc := `<location href="` + xmlEscape(m.RemoteName+"/"+href) + `"/>`
				raw = locationRe.ReplaceAll(raw, []byte(loc))
			}
			out["primary"] = append(out["primary"], raw)
		}
	}

	for _, t := range rpmDataTypes[1:] {
		emitted := map[string]bool{}
		for i, m := range members {
			pkgs, err := splitPackages(m.Data[t])
			if err != nil {
				return nil, fmt.Errorf("member %q %s: %w", m.RemoteName, t, err)
			}
			for _, raw := range pkgs {
				var p pkgidPkg
				if err := xml.Unmarshal(raw, &p); err != nil {
					return nil, fmt.Errorf("member %q %s package: %w", m.RemoteName, t, err)
				}
				key := fmt.Sprintf("%d/%s", i, p.PkgID)
				if kept[key] && !emitted[key] {
					emitted[key] = true
					out[t] = append(out[t], raw)
				}
			}
		}
	}

	var ts int64
	for _, m := range members {
		ts = max(ts, m.Timestamp)
	}

	repo := &RPMRepo{Files: map[string][]byte{}}
	var md bytes.Buffer
	md.WriteString(xml.Header)
	md.WriteString(`<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n")
	fmt.Fprintf(&md, "  <revision>%d</revision>\n", ts)
	for _, t := range rpmDataTypes {
		var doc bytes.Buffer
		doc.WriteString(xml.Header)
		fmt.Fprintf(&doc, rpmRoots[t]+"\n", len(out[t]))
		for _, raw := range out[t] {
			doc.Write(raw)
			doc.WriteString("\n")
		}
		doc.WriteString(rpmRootClose[t] + "\n")

		gz := gzipDeterministic(doc.Bytes())
		sum := sha256Hex(gz)
		href := fmt.Sprintf("repodata/%s-%s.xml.gz", sum, t)
		repo.Files[href] = gz

		fmt.Fprintf(&md, "  <data type=\"%s\">\n", t)
		fmt.Fprintf(&md, "    <checksum type=\"sha256\">%s</checksum>\n", sum)
		fmt.Fprintf(&md, "    <open-checksum type=\"sha256\">%s</open-checksum>\n", sha256Hex(doc.Bytes()))
		fmt.Fprintf(&md, "    <location href=\"%s\"/>\n", href)
		fmt.Fprintf(&md, "    <timestamp>%d</timestamp>\n", ts)
		fmt.Fprintf(&md, "    <size>%d</size>\n", len(gz))
		fmt.Fprintf(&md, "    <open-size>%d</open-size>\n", doc.Len())
		md.WriteString("  </data>\n")
	}
	md.WriteString("</repomd>\n")
	repo.Repomd = md.Bytes()
	return repo, nil
}

func gzipDeterministic(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Header = gzip.Header{OS: 255}
	_, _ = gz.Write(data)
	_ = gz.Close()
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
