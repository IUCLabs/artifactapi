package models

import "testing"

func TestRemote_ValidatePatterns(t *testing.T) {
	valid := &Remote{
		Patterns:          []string{`.*\.tar\.gz$`},
		Blocklist:         []string{`^secret/`},
		ImmutablePatterns: []string{`\.rpm$`},
	}
	if err := valid.ValidatePatterns(); err != nil {
		t.Fatalf("expected valid patterns, got %v", err)
	}

	bad := &Remote{Blocklist: []string{`[unterminated`}}
	if err := bad.ValidatePatterns(); err == nil {
		t.Fatal("expected error for invalid blocklist regex, got nil")
	}
}
