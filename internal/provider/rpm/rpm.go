package rpm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	rpmlib "github.com/cavaliergopher/rpm"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

var mutableRe = []*regexp.Regexp{
	regexp.MustCompile(`repomd\.xml$`),
	regexp.MustCompile(`repodata/`),
	regexp.MustCompile(`Packages\.gz$`),
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageRPM }

func (p *Provider) Classify(path string) provider.Mutability {
	for _, re := range mutableRe {
		if re.MatchString(path) {
			return provider.Mutable
		}
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".rpm") {
		return "application/x-rpm"
	}
	if strings.HasSuffix(path, ".xml") || strings.HasSuffix(path, ".xml.gz") || strings.HasSuffix(path, ".xml.xz") {
		return "application/xml"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(_ []byte, _ models.Remote, _ string) ([]byte, error) {
	return nil, nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	filename := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		filename = filePath[idx+1:]
	}

	if !strings.HasSuffix(strings.ToLower(filename), ".rpm") {
		return "", "", fmt.Errorf("file must be an .rpm package")
	}

	return "Packages/" + filename, "application/x-rpm", nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	filename := strings.TrimPrefix(storagePath, "Packages/")
	return map[string]any{
		"filename":     filename,
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

func (p *Provider) AfterUpload(ctx context.Context, repoName, storagePath, contentHash string, blobs provider.BlobReader, db provider.MetadataStore) {
	s3Key := storage.BlobKey(strings.TrimPrefix(contentHash, "sha256:"))

	reader, blobSize, err := blobs.Download(ctx, s3Key)
	if err != nil {
		slog.Error("rpm metadata: download failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	defer reader.Close()

	pkg, err := rpmlib.Read(reader)
	if err != nil {
		slog.Error("rpm metadata: parse failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}

	meta := &provider.RPMMetadata{
		RepoName:      repoName,
		FilePath:      storagePath,
		ContentHash:   contentHash,
		Name:          pkg.Name(),
		Epoch:         pkg.Epoch(),
		Version:       pkg.Version(),
		Release:       pkg.Release(),
		Arch:          pkg.Architecture(),
		Summary:       pkg.Summary(),
		Description:   pkg.Description(),
		RPMSize:       blobSize,
		InstalledSize: int64(pkg.Size()),
		License:       pkg.License(),
		Vendor:        pkg.Vendor(),
		Group:         firstGroup(pkg.Groups()),
		BuildHost:     pkg.BuildHost(),
		SourceRPM:     pkg.SourceRPM(),
		URL:           pkg.URL(),
		Packager:      pkg.Packager(),
	}

	for _, req := range pkg.Requires() {
		meta.Requires = append(meta.Requires, rpmDepFromEntry(req))
	}
	for _, prov := range pkg.Provides() {
		meta.Provides = append(meta.Provides, rpmDepFromEntry(prov))
	}

	if meta.Requires == nil {
		meta.Requires = []provider.RPMDep{}
	}
	if meta.Provides == nil {
		meta.Provides = []provider.RPMDep{}
	}
	meta.Files = []provider.RPMFile{}
	meta.Changelogs = []provider.RPMChangelog{}

	if err := db.InsertRPMMetadata(ctx, meta); err != nil {
		slog.Error("rpm metadata: insert failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}

	slog.Info("rpm metadata: parsed", "repo", repoName, "name", meta.Name, "version", meta.Version, "arch", meta.Arch)
}

func rpmDepFromEntry(e rpmlib.Dependency) provider.RPMDep {
	dep := provider.RPMDep{Name: e.Name()}
	if e.Flags() != 0 {
		dep.Flags = rpmFlagString(e.Flags())
		dep.Version = e.Version()
		dep.Release = e.Release()
		if e.Epoch() > 0 {
			dep.Epoch = fmt.Sprintf("%d", e.Epoch())
		}
	}
	return dep
}

func rpmFlagString(f int) string {
	switch {
	case f&0x08 != 0 && f&0x04 != 0:
		return "GE"
	case f&0x02 != 0 && f&0x04 != 0:
		return "LE"
	case f&0x08 != 0:
		return "GT"
	case f&0x02 != 0:
		return "LT"
	case f&0x04 != 0:
		return "EQ"
	default:
		return ""
	}
}

func firstGroup(groups []string) string {
	if len(groups) > 0 {
		return groups[0]
	}
	return "Unspecified"
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, path string) bool {
	if !strings.HasPrefix(path, "repodata/") {
		return false
	}

	rpmReader, ok := files.(provider.RPMMetadataReader)
	if !ok {
		http.Error(w, "rpm metadata not available", http.StatusInternalServerError)
		return true
	}

	tail := strings.TrimPrefix(path, "repodata/")

	switch {
	case tail == "repomd.xml":
		p.serveRepomd(w, r, rpmReader, repoName)
	case strings.HasSuffix(tail, "-primary.xml.gz"):
		p.servePrimary(w, r, rpmReader, repoName)
	case strings.HasSuffix(tail, "-filelists.xml.gz"):
		p.serveFilelists(w, r, rpmReader, repoName)
	case strings.HasSuffix(tail, "-other.xml.gz"):
		p.serveOther(w, r, rpmReader, repoName)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
	return true
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	return nil, fmt.Errorf("rpm local index generation for virtual repos not supported")
}

func (p *Provider) serveRepomd(w http.ResponseWriter, r *http.Request, reader provider.RPMMetadataReader, repoName string) {
	metas, err := reader.ListRPMMetadataEntries(r.Context(), repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	primary := generatePrimaryXMLGZ(metas)
	filelists := generateFilelistsXMLGZ(metas)
	other := generateOtherXMLGZ(metas)

	primaryHash := sha256Hex(primary)
	filelistsHash := sha256Hex(filelists)
	otherHash := sha256Hex(other)

	repomd := generateRepomd(primaryHash, len(primary), filelistsHash, len(filelists), otherHash, len(other))

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(repomd)
}

func (p *Provider) servePrimary(w http.ResponseWriter, r *http.Request, reader provider.RPMMetadataReader, repoName string) {
	metas, err := reader.ListRPMMetadataEntries(r.Context(), repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	w.Write(generatePrimaryXMLGZ(metas))
}

func (p *Provider) serveFilelists(w http.ResponseWriter, r *http.Request, reader provider.RPMMetadataReader, repoName string) {
	metas, err := reader.ListRPMMetadataEntries(r.Context(), repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	w.Write(generateFilelistsXMLGZ(metas))
}

func (p *Provider) serveOther(w http.ResponseWriter, r *http.Request, reader provider.RPMMetadataReader, repoName string) {
	metas, err := reader.ListRPMMetadataEntries(r.Context(), repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	w.Write(generateOtherXMLGZ(metas))
}

func generateRepomd(primaryHash string, primarySize int, filelistsHash string, filelistsSize int, otherHash string, otherSize int) []byte {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n")
	fmt.Fprintf(&b, "  <revision>%s</revision>\n", ts)

	writeRepomdData(&b, "primary", primaryHash, primarySize, ts)
	writeRepomdData(&b, "filelists", filelistsHash, filelistsSize, ts)
	writeRepomdData(&b, "other", otherHash, otherSize, ts)

	b.WriteString("</repomd>\n")
	return b.Bytes()
}

func writeRepomdData(b *bytes.Buffer, dtype, hash string, size int, ts string) {
	fmt.Fprintf(b, "  <data type=\"%s\">\n", dtype)
	fmt.Fprintf(b, "    <checksum type=\"sha256\">%s</checksum>\n", hash)
	fmt.Fprintf(b, "    <location href=\"repodata/%s-%s.xml.gz\"/>\n", hash, dtype)
	fmt.Fprintf(b, "    <timestamp>%s</timestamp>\n", ts)
	fmt.Fprintf(b, "    <size>%d</size>\n", size)
	fmt.Fprintf(b, "  </data>\n")
}

func generatePrimaryXMLGZ(metas []provider.RPMMetadata) []byte {
	var xmlBuf bytes.Buffer
	xmlBuf.WriteString(xml.Header)
	fmt.Fprintf(&xmlBuf, "<metadata xmlns=\"http://linux.duke.edu/metadata/common\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\" packages=\"%d\">\n", len(metas))

	for _, m := range metas {
		pkgHash := strings.TrimPrefix(m.ContentHash, "sha256:")
		fmt.Fprintf(&xmlBuf, "<package type=\"rpm\">\n")
		fmt.Fprintf(&xmlBuf, "  <name>%s</name>\n", xmlEscape(m.Name))
		fmt.Fprintf(&xmlBuf, "  <arch>%s</arch>\n", xmlEscape(m.Arch))
		fmt.Fprintf(&xmlBuf, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n", m.Epoch, xmlEscape(m.Version), xmlEscape(m.Release))
		fmt.Fprintf(&xmlBuf, "  <checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>\n", pkgHash)
		fmt.Fprintf(&xmlBuf, "  <summary>%s</summary>\n", xmlEscape(m.Summary))
		fmt.Fprintf(&xmlBuf, "  <description>%s</description>\n", xmlEscape(m.Description))
		if m.Packager != "" {
			fmt.Fprintf(&xmlBuf, "  <packager>%s</packager>\n", xmlEscape(m.Packager))
		}
		if m.URL != "" {
			fmt.Fprintf(&xmlBuf, "  <url>%s</url>\n", xmlEscape(m.URL))
		}
		fmt.Fprintf(&xmlBuf, "  <time file=\"%d\" build=\"0\"/>\n", time.Now().Unix())
		fmt.Fprintf(&xmlBuf, "  <size package=\"%d\" installed=\"%d\" archive=\"0\"/>\n", m.RPMSize, m.InstalledSize)
		fmt.Fprintf(&xmlBuf, "  <location href=\"%s\"/>\n", xmlEscape(m.FilePath))
		fmt.Fprintf(&xmlBuf, "  <format>\n")
		if m.License != "" {
			fmt.Fprintf(&xmlBuf, "    <rpm:license>%s</rpm:license>\n", xmlEscape(m.License))
		}
		if m.Vendor != "" {
			fmt.Fprintf(&xmlBuf, "    <rpm:vendor>%s</rpm:vendor>\n", xmlEscape(m.Vendor))
		}
		fmt.Fprintf(&xmlBuf, "    <rpm:group>%s</rpm:group>\n", xmlEscape(m.Group))
		if m.BuildHost != "" {
			fmt.Fprintf(&xmlBuf, "    <rpm:buildhost>%s</rpm:buildhost>\n", xmlEscape(m.BuildHost))
		}
		if m.SourceRPM != "" {
			fmt.Fprintf(&xmlBuf, "    <rpm:sourcerpm>%s</rpm:sourcerpm>\n", xmlEscape(m.SourceRPM))
		}

		if len(m.Provides) > 0 {
			xmlBuf.WriteString("    <rpm:provides>\n")
			for _, d := range m.Provides {
				writeRPMEntry(&xmlBuf, d)
			}
			xmlBuf.WriteString("    </rpm:provides>\n")
		}
		if len(m.Requires) > 0 {
			xmlBuf.WriteString("    <rpm:requires>\n")
			for _, d := range m.Requires {
				writeRPMEntry(&xmlBuf, d)
			}
			xmlBuf.WriteString("    </rpm:requires>\n")
		}

		fmt.Fprintf(&xmlBuf, "  </format>\n")
		fmt.Fprintf(&xmlBuf, "</package>\n")
	}
	xmlBuf.WriteString("</metadata>\n")

	return gzipBytes(xmlBuf.Bytes())
}

func generateFilelistsXMLGZ(metas []provider.RPMMetadata) []byte {
	var xmlBuf bytes.Buffer
	xmlBuf.WriteString(xml.Header)
	fmt.Fprintf(&xmlBuf, "<filelists xmlns=\"http://linux.duke.edu/metadata/filelists\" packages=\"%d\">\n", len(metas))

	for _, m := range metas {
		pkgHash := strings.TrimPrefix(m.ContentHash, "sha256:")
		fmt.Fprintf(&xmlBuf, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n", pkgHash, xmlEscape(m.Name), xmlEscape(m.Arch))
		fmt.Fprintf(&xmlBuf, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n", m.Epoch, xmlEscape(m.Version), xmlEscape(m.Release))
		for _, f := range m.Files {
			if f.Type != "" {
				fmt.Fprintf(&xmlBuf, "  <file type=\"%s\">%s</file>\n", f.Type, xmlEscape(f.Path))
			} else {
				fmt.Fprintf(&xmlBuf, "  <file>%s</file>\n", xmlEscape(f.Path))
			}
		}
		xmlBuf.WriteString("</package>\n")
	}
	xmlBuf.WriteString("</filelists>\n")

	return gzipBytes(xmlBuf.Bytes())
}

func generateOtherXMLGZ(metas []provider.RPMMetadata) []byte {
	var xmlBuf bytes.Buffer
	xmlBuf.WriteString(xml.Header)
	fmt.Fprintf(&xmlBuf, "<otherdata xmlns=\"http://linux.duke.edu/metadata/other\" packages=\"%d\">\n", len(metas))

	for _, m := range metas {
		pkgHash := strings.TrimPrefix(m.ContentHash, "sha256:")
		fmt.Fprintf(&xmlBuf, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n", pkgHash, xmlEscape(m.Name), xmlEscape(m.Arch))
		fmt.Fprintf(&xmlBuf, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n", m.Epoch, xmlEscape(m.Version), xmlEscape(m.Release))
		for _, cl := range m.Changelogs {
			fmt.Fprintf(&xmlBuf, "  <changelog author=\"%s\" date=\"%d\">%s</changelog>\n",
				xmlEscape(cl.Author), cl.Date, xmlEscape(cl.Text))
		}
		xmlBuf.WriteString("</package>\n")
	}
	xmlBuf.WriteString("</otherdata>\n")

	return gzipBytes(xmlBuf.Bytes())
}

func writeRPMEntry(b *bytes.Buffer, d provider.RPMDep) {
	if d.Flags != "" {
		fmt.Fprintf(b, "      <rpm:entry name=\"%s\" flags=\"%s\"", xmlEscape(d.Name), d.Flags)
		if d.Epoch != "" {
			fmt.Fprintf(b, " epoch=\"%s\"", d.Epoch)
		}
		if d.Version != "" {
			fmt.Fprintf(b, " ver=\"%s\"", xmlEscape(d.Version))
		}
		if d.Release != "" {
			fmt.Fprintf(b, " rel=\"%s\"", xmlEscape(d.Release))
		}
		b.WriteString("/>\n")
	} else {
		fmt.Fprintf(b, "      <rpm:entry name=\"%s\"/>\n", xmlEscape(d.Name))
	}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(data)
	gz.Close()
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
