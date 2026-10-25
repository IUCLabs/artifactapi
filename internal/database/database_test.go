package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var (
	testDB  *DB
	testDSN string
)

func TestMain(m *testing.M) {
	c := context.Background()
	dsn, terminate, err := testsupport.StartPostgres(c)
	if err != nil {
		// Docker unavailable: run anyway so tests self-skip via requireDB.
		os.Exit(m.Run())
	}
	testDSN = dsn
	db, err := New(dsn)
	if err != nil {
		terminate()
		panic(err)
	}
	testDB = db

	code := m.Run()

	db.Close()
	terminate()
	// Return normally on success so the coverage profile is flushed; os.Exit
	// would truncate it.
	if code != 0 {
		os.Exit(code)
	}
}

func requireDB(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("Docker unavailable; skipping database integration test")
	}
}

func ctx() context.Context { return context.Background() }

func seedRemote(t *testing.T, name string) {
	t.Helper()
	if err := testDB.CreateRemote(ctx(), &models.Remote{
		Name: name, PackageType: models.PackageGeneric, RepoType: models.RepoTypeRemote,
		BaseURL: "https://example.com", MutableTTL: 3600,
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
}

// seedBlob inserts a blob and returns its full content hash (sha256:<hash>),
// matching the reference convention used by artifacts and local files.
func seedBlob(t *testing.T, hash string) string {
	t.Helper()
	full := "sha256:" + hash
	if err := testDB.UpsertBlob(ctx(), full, "blobs/sha256/"+hash, 10, "application/octet-stream"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	return full
}

func TestRemotesCRUD(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-crud")
	got, err := testDB.GetRemote(ctx(), "r-crud")
	if err != nil || got.BaseURL != "https://example.com" {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.CacheRepodata {
		t.Error("cache_repodata should default to false")
	}
	got.BaseURL = "https://updated.example.com"
	got.CacheRepodata = true
	got.SyncInterval = 600
	got.ServeStale = true
	if err := testDB.UpdateRemote(ctx(), got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = testDB.GetRemote(ctx(), "r-crud")
	if got.BaseURL != "https://updated.example.com" || !got.CacheRepodata || got.SyncInterval != 600 || !got.ServeStale {
		t.Errorf("update not applied: %v %v %v %v", got.BaseURL, got.CacheRepodata, got.SyncInterval, got.ServeStale)
	}
	list, err := testDB.ListRemotes(ctx())
	if err != nil || len(list) == 0 {
		t.Fatalf("list: %v %v", len(list), err)
	}
	if err := testDB.DeleteRemote(ctx(), "r-crud"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := testDB.GetRemote(ctx(), "r-crud"); err == nil {
		t.Error("expected error after delete")
	}
}

func TestRemoteMirrorlistRoundTrip(t *testing.T) {
	requireDB(t)
	mirrors := []string{"https://b.example", "https://c.example"}
	if err := testDB.CreateRemote(ctx(), &models.Remote{
		Name: "r-mirror", PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote,
		BaseURL: "https://a.example", Mirrorlist: mirrors, MutableTTL: 3600,
	}); err != nil {
		t.Fatalf("create mirrorlist remote: %v", err)
	}
	defer testDB.DeleteRemote(ctx(), "r-mirror")

	got, err := testDB.GetRemote(ctx(), "r-mirror")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BaseURL != "https://a.example" {
		t.Fatalf("BaseURL = %q, want https://a.example", got.BaseURL)
	}
	if len(got.Mirrorlist) != 2 || got.Mirrorlist[0] != mirrors[0] || got.Mirrorlist[1] != mirrors[1] {
		t.Fatalf("Mirrorlist round-trip = %v, want %v", got.Mirrorlist, mirrors)
	}
	// An unset strategy is stored as the round_robin default.
	if got.MirrorStrategy != models.MirrorStrategyRoundRobin {
		t.Fatalf("MirrorStrategy default = %q, want %q", got.MirrorStrategy, models.MirrorStrategyRoundRobin)
	}

	// Updating to least_conn round-trips.
	got.MirrorStrategy = models.MirrorStrategyLeastConn
	if err := testDB.UpdateRemote(ctx(), got); err != nil {
		t.Fatalf("update to least_conn: %v", err)
	}
	got, _ = testDB.GetRemote(ctx(), "r-mirror")
	if got.MirrorStrategy != models.MirrorStrategyLeastConn {
		t.Fatalf("MirrorStrategy after update = %q, want least_conn", got.MirrorStrategy)
	}

	// Clearing the mirrorlist on update persists an empty list.
	got.Mirrorlist = nil
	if err := testDB.UpdateRemote(ctx(), got); err != nil {
		t.Fatalf("update clearing mirrorlist: %v", err)
	}
	got, _ = testDB.GetRemote(ctx(), "r-mirror")
	if len(got.Mirrorlist) != 0 {
		t.Fatalf("mirrorlist after clear = %v, want empty", got.Mirrorlist)
	}
}

func TestArtifactsAndBlobs(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-art")
	seedBlob(t, "aaaa")
	hash := "sha256:aaaa"
	if err := testDB.UpsertBlob(ctx(), hash, "blobs/sha256/aaaa", 10, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := testDB.UpsertArtifact(ctx(), "r-art", "path/a.txt", hash, "etag1"); err != nil {
		t.Fatal(err)
	}
	// Upsert again to exercise the ON CONFLICT update branch.
	if err := testDB.UpsertArtifact(ctx(), "r-art", "path/a.txt", hash, "etag2"); err != nil {
		t.Fatal(err)
	}
	art, err := testDB.GetArtifact(ctx(), "r-art", "path/a.txt")
	if err != nil || art.ContentHash != hash {
		t.Fatalf("get artifact: %v %v", art, err)
	}
	if err := testDB.TouchArtifactAccess(ctx(), "r-art", "path/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := testDB.UpsertArtifact(ctx(), "r-art", "other/b.txt", hash, ""); err != nil {
		t.Fatal(err)
	}
	arts, err := testDB.ListArtifacts(ctx(), "r-art", "", 10, 0)
	if err != nil || len(arts) != 2 {
		t.Fatalf("list artifacts: %v %v", len(arts), err)
	}
	arts, err = testDB.ListArtifacts(ctx(), "r-art", "path/", 10, 0)
	if err != nil || len(arts) != 1 || arts[0].Path != "path/a.txt" {
		t.Fatalf("list artifacts with prefix: %+v %v", arts, err)
	}
	if err := testDB.InsertAccessLog(ctx(), "r-art", "path/a.txt", true, 10, 5, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if err := testDB.InsertAccessLogBatch(ctx(), []AccessLogEntry{
		{RemoteName: "r-art", Path: "b", CacheHit: false, SizeBytes: 20, UpstreamMS: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := testDB.InsertAccessLogBatch(ctx(), nil); err != nil {
		t.Fatalf("empty batch should be a no-op: %v", err)
	}
	if err := testDB.DeleteArtifact(ctx(), "r-art", "path/a.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestListArtifactsPrefix(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-prefix")
	seedBlob(t, "prefixhash")
	for _, p := range []string{"a%b/y", "a1b/y", "a_b/x", "aXb/x", "pkg/1", "pkg/2", "pkg/3", "pkgx/4"} {
		if err := testDB.UpsertArtifact(ctx(), "r-prefix", p, "sha256:prefixhash", ""); err != nil {
			t.Fatal(err)
		}
	}
	paths := func(prefix string, limit, offset int) []string {
		t.Helper()
		arts, err := testDB.ListArtifacts(ctx(), "r-prefix", prefix, limit, offset)
		if err != nil {
			t.Fatalf("list %q: %v", prefix, err)
		}
		out := make([]string, len(arts))
		for i, a := range arts {
			out[i] = a.Path
		}
		return out
	}

	// LIKE wildcards in the prefix must match literally.
	if got := paths("a_b/", 10, 0); len(got) != 1 || got[0] != "a_b/x" {
		t.Fatalf("prefix a_b/ = %v, want [a_b/x]", got)
	}
	if got := paths("a%", 10, 0); len(got) != 1 || got[0] != "a%b/y" {
		t.Fatalf("prefix a%% = %v, want [a%%b/y]", got)
	}

	// limit/offset page the filtered set, not the whole remote.
	if got := paths("pkg/", 2, 0); len(got) != 2 || got[0] != "pkg/1" || got[1] != "pkg/2" {
		t.Fatalf("prefix pkg/ page 1 = %v, want [pkg/1 pkg/2]", got)
	}
	if got := paths("pkg/", 2, 2); len(got) != 1 || got[0] != "pkg/3" {
		t.Fatalf("prefix pkg/ page 2 = %v, want [pkg/3]", got)
	}
}

func TestOrphanAndColdCleanup(t *testing.T) {
	requireDB(t)
	seedBlob(t, "orphanhash")
	// A blob with no artifact/local_file reference is orphaned, but only past
	// the grace period.
	if got, _ := testDB.FindOrphanedBlobs(ctx(), time.Hour); containsHash(got, "sha256:orphanhash") {
		t.Error("fresh orphan should be excluded by grace period")
	}
	orphans, err := testDB.FindOrphanedBlobs(ctx(), -time.Hour) // cutoff in the future => include fresh
	if err != nil {
		t.Fatal(err)
	}
	if !containsHash(orphans, "sha256:orphanhash") {
		t.Error("expected orphan to be found with zero grace")
	}
	if err := testDB.DeleteBlob(ctx(), "sha256:orphanhash"); err != nil {
		t.Fatal(err)
	}

	seedRemote(t, "r-cold")
	seedBlob(t, "coldhash")
	testDB.UpsertArtifact(ctx(), "r-cold", "cold.txt", "sha256:coldhash", "")
	n, err := testDB.DeleteColdArtifacts(ctx(), "r-cold", -time.Hour) // negative => everything is "cold"
	if err != nil || n < 1 {
		t.Fatalf("delete cold: n=%d err=%v", n, err)
	}
}

func containsHash(blobs []models.Blob, hash string) bool {
	for _, b := range blobs {
		if b.ContentHash == hash {
			return true
		}
	}
	return false
}

func TestLocalFiles(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-local")
	seedBlob(t, "localhash")
	hash := "sha256:localhash"
	if err := testDB.CreateLocalFile(ctx(), "r-local", "foo/foo-1.0.whl", hash); err != nil {
		t.Fatal(err)
	}
	// Duplicate create must be rejected.
	if err := testDB.CreateLocalFile(ctx(), "r-local", "foo/foo-1.0.whl", hash); err == nil {
		t.Error("expected duplicate local file error")
	}
	f, err := testDB.GetLocalFile(ctx(), "r-local", "foo/foo-1.0.whl")
	if err != nil || f == nil {
		t.Fatalf("get local file: %v %v", f, err)
	}
	if files, err := testDB.ListLocalFiles(ctx(), "r-local", 10, 0); err != nil || len(files) != 1 {
		t.Fatalf("list: %v %v", len(files), err)
	}
	if files, err := testDB.ListLocalFilesByPrefix(ctx(), "r-local", "foo/"); err != nil || len(files) != 1 {
		t.Fatalf("list by prefix: %v %v", len(files), err)
	}
	if entries, err := testDB.ListFilesByPrefix(ctx(), "r-local", "foo/"); err != nil || len(entries) != 1 {
		t.Fatalf("provider list by prefix: %v %v", len(entries), err)
	}
	if pkgs, err := testDB.ListLocalFilePackages(ctx(), "r-local"); err != nil || len(pkgs) == 0 {
		t.Fatalf("list packages: %v %v", pkgs, err)
	}
	if pkgs, err := testDB.ListPackages(ctx(), "r-local"); err != nil || len(pkgs) == 0 {
		t.Fatalf("provider list packages: %v %v", pkgs, err)
	}
	if err := testDB.DeleteLocalFile(ctx(), "r-local", "foo/foo-1.0.whl"); err != nil {
		t.Fatal(err)
	}
}

// A repo recreated under a deleted repo's name must not inherit its files or
// metadata, or re-uploading the same path 409s.
func TestDeleteRemoteDropsLocalFiles(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-recreate")
	hash := seedBlob(t, "recreatehash")
	if err := testDB.CreateLocalFile(ctx(), "r-recreate", "x86_64/a-1.0-r0.apk", hash); err != nil {
		t.Fatal(err)
	}
	if err := testDB.InsertAlpineMetadata(ctx(), &provider.AlpineMetadata{RepoName: "r-recreate", FilePath: "x86_64/a-1.0-r0.apk", ContentHash: hash, Name: "a", Version: "1.0-r0", Arch: "x86_64"}); err != nil {
		t.Fatal(err)
	}
	if err := testDB.DeleteRemote(ctx(), "r-recreate"); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, "r-recreate")
	defer testDB.DeleteRemote(ctx(), "r-recreate")
	if f, err := testDB.GetLocalFile(ctx(), "r-recreate", "x86_64/a-1.0-r0.apk"); err != nil || f != nil {
		t.Fatalf("recreated repo inherited local file: %v %v", f, err)
	}
	if rows, err := testDB.ListAlpineMetadataEntries(ctx(), "r-recreate"); err != nil || len(rows) != 0 {
		t.Fatalf("recreated repo inherited alpine metadata: %v %v", rows, err)
	}
	if err := testDB.CreateLocalFile(ctx(), "r-recreate", "x86_64/a-1.0-r0.apk", hash); err != nil {
		t.Fatalf("re-upload after recreate: %v", err)
	}
}

func TestVirtualParallelFetchRoundTrip(t *testing.T) {
	requireDB(t)
	if err := testDB.CreateVirtual(ctx(), &models.Virtual{
		Name: "v-parallel", PackageType: models.PackageRPM, Members: []string{"a"}, ParallelFetch: true,
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testDB.DeleteVirtual(ctx(), "v-parallel") }()
	v, err := testDB.GetVirtual(ctx(), "v-parallel")
	if err != nil || !v.ParallelFetch {
		t.Fatalf("created parallel_fetch = %v, %v; want true", v, err)
	}
	v.ParallelFetch = false
	if err := testDB.UpdateVirtual(ctx(), v); err != nil {
		t.Fatal(err)
	}
	vs, err := testDB.ListVirtuals(ctx())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range vs {
		if l.Name == "v-parallel" && l.ParallelFetch {
			t.Fatal("updated parallel_fetch not persisted")
		}
	}
	if err := testDB.CreateVirtual(ctx(), &models.Virtual{Name: "v-serial", PackageType: models.PackageRPM, Members: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testDB.DeleteVirtual(ctx(), "v-serial") }()
	if v, err := testDB.GetVirtual(ctx(), "v-serial"); err != nil || v.ParallelFetch {
		t.Fatalf("default parallel_fetch = %v, %v; want false", v, err)
	}
}

func TestVirtualsCRUD(t *testing.T) {
	requireDB(t)
	if err := testDB.CreateVirtual(ctx(), &models.Virtual{
		Name: "v-crud", PackageType: models.PackageHelm, Members: []string{"a", "b"},
	}); err != nil {
		t.Fatal(err)
	}
	v, err := testDB.GetVirtual(ctx(), "v-crud")
	if err != nil || len(v.Members) != 2 {
		t.Fatalf("get virtual: %v %v", v, err)
	}
	v.Members = []string{"a"}
	if err := testDB.UpdateVirtual(ctx(), v); err != nil {
		t.Fatal(err)
	}
	if vs, err := testDB.ListVirtuals(ctx()); err != nil || len(vs) == 0 {
		t.Fatalf("list virtuals: %v %v", len(vs), err)
	}
	if err := testDB.DeleteVirtual(ctx(), "v-crud"); err != nil {
		t.Fatal(err)
	}
}

func TestStats(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-stats")
	seedBlob(t, "statshash")
	testDB.UpsertArtifact(ctx(), "r-stats", "s.txt", "sha256:statshash", "")
	testDB.InsertAccessLog(ctx(), "r-stats", "s.txt", true, 100, 2, "")

	if _, err := testDB.GetOverviewStats(ctx()); err != nil {
		t.Fatalf("overview: %v", err)
	}
	if _, err := testDB.GetTopRemotes(ctx(), 5); err != nil {
		t.Fatalf("top remotes: %v", err)
	}
	if _, err := testDB.GetTopFilesByHits(ctx(), 5); err != nil {
		t.Fatalf("top files by hits: %v", err)
	}
	if _, err := testDB.GetTopFilesByBandwidth(ctx(), 5); err != nil {
		t.Fatalf("top files by bandwidth: %v", err)
	}
}

func TestDatabaseErrorPaths(t *testing.T) {
	requireDB(t)
	bad, err := New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	bad.Close() // every query now fails
	ctx := context.Background()

	if _, err := bad.ListRemotes(ctx); err == nil {
		t.Error("ListRemotes should error on closed db")
	}
	if _, err := bad.ListVirtuals(ctx); err == nil {
		t.Error("ListVirtuals should error")
	}
	if _, err := bad.ListArtifacts(ctx, "r", "", 10, 0); err == nil {
		t.Error("ListArtifacts should error")
	}
	if _, err := bad.ListLocalFiles(ctx, "r", 10, 0); err == nil {
		t.Error("ListLocalFiles should error")
	}
	if _, err := bad.ListLocalFilesByPrefix(ctx, "r", "p"); err == nil {
		t.Error("ListLocalFilesByPrefix should error")
	}
	if _, err := bad.ListLocalFilePackages(ctx, "r"); err == nil {
		t.Error("ListLocalFilePackages should error")
	}
	if _, err := bad.ListFilesByPrefix(ctx, "r", "p"); err == nil {
		t.Error("ListFilesByPrefix should error")
	}
	if _, err := bad.ListPackages(ctx, "r"); err == nil {
		t.Error("ListPackages should error")
	}
	if _, err := bad.FindOrphanedBlobs(ctx, 0); err == nil {
		t.Error("FindOrphanedBlobs should error")
	}
	if _, err := bad.GetOverviewStats(ctx); err == nil {
		t.Error("GetOverviewStats should error")
	}
	if _, err := bad.GetTopRemotes(ctx, 5); err == nil {
		t.Error("GetTopRemotes should error")
	}
	if _, err := bad.GetTopFilesByHits(ctx, 5); err == nil {
		t.Error("GetTopFilesByHits should error")
	}
	if _, err := bad.GetTopFilesByBandwidth(ctx, 5); err == nil {
		t.Error("GetTopFilesByBandwidth should error")
	}
	if _, err := bad.ListRPMMetadataEntries(ctx, "r"); err == nil {
		t.Error("ListRPMMetadataEntries should error")
	}
}

func TestRPMMetadata(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-rpm")
	meta := &provider.RPMMetadata{
		RepoName: "r-rpm", FilePath: "Packages/x.rpm", ContentHash: "sha256:rpm",
		Name: "x", Version: "1.0", Release: "1", Arch: "noarch",
		Requires: []provider.RPMDep{{Name: "libc"}},
		Provides: []provider.RPMDep{{Name: "x"}},
		Files:    []provider.RPMFile{},
	}
	if err := testDB.InsertRPMMetadata(ctx(), meta); err != nil {
		t.Fatal(err)
	}
	entries, err := testDB.ListRPMMetadataEntries(ctx(), "r-rpm")
	if err != nil || len(entries) != 1 {
		t.Fatalf("list rpm entries: %v %v", len(entries), err)
	}
	if rows, err := testDB.ListRPMMetadata(ctx(), "r-rpm"); err != nil || len(rows) != 1 {
		t.Fatalf("list rpm rows: %v %v", len(rows), err)
	}
}

func TestRPMMetadataFingerprint(t *testing.T) {
	requireDB(t)
	seedRemote(t, "r-rpm-fp")
	fp := func() [2]int64 {
		t.Helper()
		c, m, err := testDB.RPMMetadataFingerprint(ctx(), "r-rpm-fp")
		if err != nil {
			t.Fatal(err)
		}
		return [2]int64{c, m}
	}
	insert := func(path string) {
		t.Helper()
		if err := testDB.InsertRPMMetadata(ctx(), &provider.RPMMetadata{
			RepoName: "r-rpm-fp", FilePath: path, ContentHash: "sha256:" + path,
			Name: path, Version: "1", Release: "1", Arch: "noarch",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := fp(); got != [2]int64{0, 0} {
		t.Fatalf("empty fingerprint = %v", got)
	}
	insert("a.rpm")
	one := fp()
	insert("a.rpm")
	if fp() != one {
		t.Fatal("conflicting insert changed fingerprint")
	}
	insert("b.rpm")
	two := fp()
	if err := testDB.DeleteRPMMetadata(ctx(), "r-rpm-fp", "a.rpm"); err != nil {
		t.Fatal(err)
	}
	afterDelete := fp()
	if one[0] != 1 || two[0] != 2 || afterDelete[0] != 1 || afterDelete == one || afterDelete == two {
		t.Fatalf("fingerprints did not track changes: %v %v %v", one, two, afterDelete)
	}
}
