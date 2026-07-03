package models

import "testing"

func TestRepoType(t *testing.T) {
	if RepoTypeRemote.String() != "remote" || RepoTypeLocal.String() != "local" {
		t.Error("RepoType.String")
	}
	if !RepoTypeRemote.Valid() || RepoType("bogus").Valid() {
		t.Error("RepoType.Valid")
	}
	if rt, err := ParseRepoType("local"); err != nil || rt != RepoTypeLocal {
		t.Errorf("ParseRepoType(local) = %v %v", rt, err)
	}
	if _, err := ParseRepoType("nope"); err == nil {
		t.Error("ParseRepoType should reject unknown")
	}
}
