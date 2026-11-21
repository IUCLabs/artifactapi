package database

import (
	"encoding/json"
	"testing"
)

func TestRPMEntriesFromRowsKeepsMalformed(t *testing.T) {
	rows := []RPMMetadataRow{
		{RepoName: "r", FilePath: "a.rpm", Name: "a", Files: json.RawMessage(`[{"path":"/usr/bin/a"}]`)},
		{RepoName: "r", FilePath: "bad.rpm", Name: "bad", Version: "1.0",
			Requires: json.RawMessage(`[{"name":"libc"}]`), Files: json.RawMessage(`[{"path":"/x"},{"path":1}]`)},
		{RepoName: "r", FilePath: "c.rpm", Name: "c"},
	}
	got := rpmEntriesFromRows(rows)
	if len(got) != 3 || got[0].Name != "a" || got[1].FilePath != "bad.rpm" || got[2].Name != "c" {
		t.Fatalf("got %+v, want rows a, bad and c", got)
	}
	if len(got[0].Files) != 1 {
		t.Errorf("row a files = %+v, want 1 decoded entry", got[0].Files)
	}
	bad := got[1]
	if bad.Files != nil {
		t.Errorf("bad row files = %+v, want empty", bad.Files)
	}
	if bad.Version != "1.0" || len(bad.Requires) != 1 || bad.Requires[0].Name != "libc" {
		t.Errorf("bad row good fields lost: %+v", bad)
	}
}
