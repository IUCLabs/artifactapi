package virtual

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// AlpineMember is one member's decompressed APKINDEX for a directory; Index is
// nil when the member does not serve that directory.
type AlpineMember struct {
	RemoteName string
	Index      []byte
}

// AlpineRepo is a merged directory: its APKINDEX.tar.gz plus, per
// "<name>-<version>.apk" apk will request, the member that owns it.
type AlpineRepo struct {
	Index  []byte
	Owners map[string][]byte
}

func (r *AlpineRepo) index() []byte            { return r.Index }
func (r *AlpineRepo) files() map[string][]byte { return r.Owners }

// MergeAlpine merges members' APKINDEX records. Members are in priority order:
// the first to carry a (P, V, A) wins. The index is unsigned, like the local
// and github_alpine indexes it merges.
func MergeAlpine(members []AlpineMember) *AlpineRepo {
	repo := &AlpineRepo{Owners: map[string][]byte{}}
	seen := map[string]bool{}
	var b bytes.Buffer
	for _, m := range members {
		for _, rec := range splitStanzas(m.Index) {
			p, v, a := stanzaField(rec, "P"), stanzaField(rec, "V"), stanzaField(rec, "A")
			key := p + "\x00" + v + "\x00" + a
			if p == "" || v == "" || seen[key] {
				continue
			}
			seen[key] = true
			if _, ok := repo.Owners[p+"-"+v+".apk"]; !ok {
				repo.Owners[p+"-"+v+".apk"] = []byte(m.RemoteName)
			}
			b.WriteString(rec)
			b.WriteString("\n\n")
		}
	}
	var t bytes.Buffer
	tw := tar.NewWriter(&t)
	_ = tw.WriteHeader(&tar.Header{Name: "APKINDEX", Mode: 0o644, Size: int64(b.Len()), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
	_, _ = tw.Write(b.Bytes())
	_ = tw.Close()
	repo.Index = gzipDeterministic(t.Bytes())
	return repo
}

// apkIndexText extracts the APKINDEX entry from an APKINDEX.tar.gz, skipping a
// leading signature stream if present.
func apkIndexText(body []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no APKINDEX entry")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "APKINDEX" {
			return io.ReadAll(tr)
		}
	}
}

// alpinePath splits a request into its directory and file, or ok is false for
// anything but an APKINDEX.tar.gz or .apk.
func alpinePath(p string) (dir, file string, ok bool) {
	i := strings.LastIndex(p, "/")
	dir, file = p[:i+1], p[i+1:]
	return dir, file, dir != "" && (file == "APKINDEX.tar.gz" || strings.HasSuffix(file, ".apk"))
}
