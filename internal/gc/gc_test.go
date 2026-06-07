package gc_test

import (
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/gc"
)

func TestNew(t *testing.T) {
	c := gc.New(nil, nil, 1*time.Hour)
	if c == nil {
		t.Fatal("expected non-nil collector")
	}
}
