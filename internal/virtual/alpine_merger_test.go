package virtual

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func apkRec(p, v, a string) string {
	return "C:Q1" + p + v + "\nP:" + p + "\nV:" + v + "\nA:" + a + "\nS:10\n"
}

func apkTarGz(t *testing.T, name string, body []byte, closeTar bool) []byte {
	t.Helper()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	if closeTar {
		tw.Close()
	} else {
		tw.Flush()
	}
	var gb bytes.Buffer
	gz := gzip.NewWriter(&gb)
	gz.Write(tb.Bytes())
	gz.Close()
	return gb.Bytes()
}

func TestMergeAlpine(t *testing.T) {
	repo := MergeAlpine([]AlpineMember{
		{RemoteName: "a", Index: []byte(apkRec("foo", "1.0-r0", "x86_64") + "\n" + apkRec("bar", "1-r0", "noarch") + "\nC:Q1x\nV:9\n")},
		{RemoteName: "b"},
		{RemoteName: "c", Index: []byte(apkRec("foo", "1.0-r0", "x86_64") + "\n" + apkRec("foo", "2.0-r0", "x86_64") + "\n" + apkRec("bar", "1-r0", "x86_64"))},
	})
	idx, err := apkIndexText(repo.Index)
	if err != nil {
		t.Fatal(err)
	}
	recs := splitStanzas(idx)
	if len(recs) != 4 || strings.Count(string(idx), "P:foo\nV:1.0-r0\n") != 1 {
		t.Fatalf("want foo 1.0 once, foo 2.0 and bar for both arches, record without P dropped:\n%s", idx)
	}
	for file, want := range map[string]string{"foo-1.0-r0.apk": "a", "foo-2.0-r0.apk": "c", "bar-1-r0.apk": "a"} {
		if got := string(repo.Owners[file]); got != want {
			t.Errorf("%s owned by %q, want %q", file, got, want)
		}
	}
	if again := MergeAlpine([]AlpineMember{{RemoteName: "a", Index: idx}}); !bytes.Equal(again.Index, MergeAlpine([]AlpineMember{{RemoteName: "a", Index: idx}}).Index) {
		t.Fatal("merged index must be byte-stable across merges")
	}
}

func TestApkIndexTextSkipsSignature(t *testing.T) {
	signed := append(apkTarGz(t, ".SIGN.RSA.alpine.rsa.pub", []byte("sig"), false), apkTarGz(t, "APKINDEX", []byte(apkRec("foo", "1", "x86_64")), true)...)
	idx, err := apkIndexText(signed)
	if err != nil || !strings.Contains(string(idx), "P:foo\n") {
		t.Fatalf("signed index: %v\n%s", err, idx)
	}
	if _, err := apkIndexText(apkTarGz(t, "DESCRIPTION", []byte("x"), true)); err == nil {
		t.Fatal("index without APKINDEX entry should fail")
	}
}

func alpineEngine(members map[string]string) *Engine {
	e := fakeEngine(nil)
	e.apkMember = func(_ context.Context, name, dir string) (*AlpineMember, error) {
		idx, ok := members[name]
		if !ok {
			return nil, errors.New("upstream down")
		}
		if dir != "x86_64/" {
			return &AlpineMember{RemoteName: name}, nil
		}
		return &AlpineMember{RemoteName: name, Index: []byte(idx)}, nil
	}
	return e
}

func alpineVirt(members ...string) models.Virtual {
	return models.Virtual{Name: "v", PackageType: models.PackageAlpine, Members: members}
}

func TestFetchAlpineIndexAndRedirect(t *testing.T) {
	e := alpineEngine(map[string]string{"loc": apkRec("foo", "1-r0", "x86_64"), "gh": apkRec("foo", "1-r0", "x86_64") + "\n" + apkRec("tool", "2-r0", "x86_64")})
	virt := alpineVirt("loc", "gh")

	body, ct, err := e.Fetch(context.Background(), virt, "./x86_64/APKINDEX.tar.gz", "")
	if err != nil || ct != "application/gzip" {
		t.Fatalf("fetch index: %v %q", err, ct)
	}
	if idx, _ := apkIndexText(body); strings.Count(string(idx), "P:") != 2 {
		t.Fatalf("merged index:\n%s", idx)
	}
	for _, tc := range []struct {
		path, want string
		ok         bool
	}{
		{path: "x86_64/foo-1-r0.apk", ok: true, want: "http://h/api/v1/remote/loc/x86_64/foo-1-r0.apk"},
		{path: "./x86_64/tool-2-r0.apk", ok: true, want: "http://h/api/v1/remote/gh/x86_64/tool-2-r0.apk"},
		{path: "x86_64/../x86_64/tool-2-r0.apk", ok: true, want: "http://h/api/v1/remote/gh/x86_64/tool-2-r0.apk"},
		{path: "x86_64/missing-1-r0.apk"},
		{path: "aarch64/foo-1-r0.apk"},
		{path: "x86_64/APKINDEX.tar.gz"},
		{path: "foo-1-r0.apk"},
	} {
		got, ok, err := e.MemberRedirect(context.Background(), virt, tc.path, "", "http://h/")
		if got != tc.want || ok != tc.ok || err != nil {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v)", tc.path, got, ok, err, tc.want, tc.ok)
		}
	}
	for _, p := range []string{"x86_64/missing-1-r0.apk", "x86_64/DESCRIPTION", "APKINDEX.tar.gz"} {
		if _, _, err := e.Fetch(context.Background(), virt, p, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want not found, got %v", p, err)
		}
	}
}

func TestFetchAlpineFailsClosed(t *testing.T) {
	e := alpineEngine(map[string]string{"a": apkRec("foo", "1-r0", "x86_64")})
	virt := alpineVirt("a", "down")
	if _, _, err := e.Fetch(context.Background(), virt, "x86_64/APKINDEX.tar.gz", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("index with a member down: want upstream error, got %v", err)
	}
	if _, ok, err := e.MemberRedirect(context.Background(), virt, "x86_64/foo-1-r0.apk", "", "http://h/"); ok || err != nil {
		t.Fatalf("redirect with a member down must defer to Fetch: ok %v err %v", ok, err)
	}
	if _, _, err := e.Fetch(context.Background(), virt, "x86_64/foo-1-r0.apk", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("download with a member down: want upstream error, got %v", err)
	}
}
