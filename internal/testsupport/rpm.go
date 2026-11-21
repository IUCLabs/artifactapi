package testsupport

import (
	"bytes"
	"encoding/binary"
)

// MinimalRPM builds a valid-enough RPM package in pure Go (no committed binary
// fixture, no external rpmbuild). It carries just the header tags the provider
// reads: name/version/release/arch plus a single self Provides entry, which is
// enough for cavaliergopher/rpm to parse and for repodata generation.
func MinimalRPM(name, version, release, arch string) []byte {
	type tag struct {
		id, typ, count uint32
		data           []byte
	}
	cstr := func(s string) []byte { return append([]byte(s), 0) }
	tags := []tag{
		{1000, 6, 1, cstr(name)},         // RPMTAG_NAME (STRING)
		{1001, 6, 1, cstr(version)},      // RPMTAG_VERSION
		{1002, 6, 1, cstr(release)},      // RPMTAG_RELEASE
		{1022, 6, 1, cstr(arch)},         // RPMTAG_ARCH
		{1047, 8, 1, cstr(name)},         // RPMTAG_PROVIDENAME (STRING_ARRAY)
		{1112, 4, 1, []byte{0, 0, 0, 0}}, // RPMTAG_PROVIDEFLAGS (INT32)
		{1113, 8, 1, cstr(version)},      // RPMTAG_PROVIDEVERSION (STRING_ARRAY)
	}

	buildHeader := func(entries []tag) []byte {
		var index, store bytes.Buffer
		for _, e := range entries {
			off := uint32(store.Len())
			for _, v := range []uint32{e.id, e.typ, off, e.count} {
				_ = binary.Write(&index, binary.BigEndian, v)
			}
			store.Write(e.data)
		}
		var b bytes.Buffer
		b.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0}) // header magic + reserved
		_ = binary.Write(&b, binary.BigEndian, uint32(len(entries)))
		_ = binary.Write(&b, binary.BigEndian, uint32(store.Len()))
		b.Write(index.Bytes())
		b.Write(store.Bytes())
		return b.Bytes()
	}

	lead := make([]byte, 96)
	copy(lead[0:4], []byte{0xed, 0xab, 0xee, 0xdb}) // lead magic
	lead[4] = 3                                     // major version
	binary.BigEndian.PutUint16(lead[8:10], 1)       // archnum
	copy(lead[10:76], name)                         // name (66 bytes, null-padded)
	binary.BigEndian.PutUint16(lead[76:78], 1)      // osnum
	binary.BigEndian.PutUint16(lead[78:80], 5)      // signature type

	var out bytes.Buffer
	out.Write(lead)
	out.Write(buildHeader(nil)) // empty signature header (16 bytes, 8-aligned)
	out.Write(buildHeader(tags))
	return out.Bytes()
}
