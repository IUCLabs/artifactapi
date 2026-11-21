package database

import (
	"encoding/json"
	"testing"
)

func TestRPMEntriesFromRowsSkipsMalformed(t *testing.T) {
	rows := []RPMMetadataRow{
		{RepoName: "r", FilePath: "a.rpm", Name: "a", Files: json.RawMessage(`[{"path":"/usr/bin/a"}]`)},
		{RepoName: "r", FilePath: "bad.rpm", Name: "bad", Files: json.RawMessage(`{"bad":1}`)},
		{RepoName: "r", FilePath: "c.rpm", Name: "c"},
	}
	got := rpmEntriesFromRows(rows)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("got %+v, want rows a and c", got)
	}
	if len(got[0].Files) != 1 {
		t.Errorf("row a files = %+v, want 1 decoded entry", got[0].Files)
	}
}
