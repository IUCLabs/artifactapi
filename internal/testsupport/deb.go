package testsupport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
)

// MinimalDeb builds a valid-enough Debian package in pure Go (no committed
// binary fixture, no dpkg-deb): an ar archive of debian-binary, a gzip
// control.tar.gz carrying ./control, and an (empty) gzip data.tar.gz. It is the
// deb analog of MinimalRPM and is parseable by the deb provider.
func MinimalDeb(name, version, arch string) []byte {
	control := fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: e2e <e2e@example.com>\n"+
			"Section: utils\nPriority: optional\nDescription: minimal test package\n",
		name, version, arch)

	controlTarGz := gzipBytes(tarSingle("./control", []byte(control)))
	dataTarGz := gzipBytes(tarEmpty())

	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arWrite(&buf, "debian-binary", []byte("2.0\n"))
	arWrite(&buf, "control.tar.gz", controlTarGz)
	arWrite(&buf, "data.tar.gz", dataTarGz)
	return buf.Bytes()
}

func arWrite(buf *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(buf, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(data))
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}

func tarSingle(name string, data []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
	tw.Write(data)
	tw.Close()
	return buf.Bytes()
}

func tarEmpty() []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.Close()
	return buf.Bytes()
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(data)
	gz.Close()
	return buf.Bytes()
}
