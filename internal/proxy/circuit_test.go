package proxy_test

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/proxy"
)

func TestCircuitBreaker_New(t *testing.T) {
	cb := proxy.NewCircuitBreaker(nil)
	if cb == nil {
		t.Fatal("expected non-nil circuit breaker")
	}
}
