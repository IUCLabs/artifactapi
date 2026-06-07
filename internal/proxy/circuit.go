package proxy

import (
	"context"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	defaultCircuitThreshold = 5
	defaultCircuitCooldown  = 60 * time.Second
)

type CircuitBreaker struct {
	cache     *cache.Redis
	threshold int64
	cooldown  time.Duration
}

func NewCircuitBreaker(c *cache.Redis) *CircuitBreaker {
	return &CircuitBreaker{
		cache:     c,
		threshold: defaultCircuitThreshold,
		cooldown:  defaultCircuitCooldown,
	}
}

func (cb *CircuitBreaker) IsOpen(ctx context.Context, remote string) bool {
	failures, err := cb.cache.GetCircuitFailures(ctx, remote)
	if err != nil {
		return false
	}
	return failures >= cb.threshold
}

func (cb *CircuitBreaker) RecordFailure(ctx context.Context, remote string) {
	cb.cache.IncrCircuitFailure(ctx, remote, cb.cooldown)
}

func (cb *CircuitBreaker) RecordSuccess(ctx context.Context, remote string) {
	cb.cache.ResetCircuit(ctx, remote)
}

func (cb *CircuitBreaker) Health(ctx context.Context, remote string) models.RemoteHealth {
	failures, err := cb.cache.GetCircuitFailures(ctx, remote)
	if err != nil {
		return models.RemoteHealth{Status: "unknown"}
	}

	switch {
	case failures == 0:
		return models.RemoteHealth{Status: "healthy", ConsecutiveFailures: int(failures)}
	case failures < cb.threshold:
		return models.RemoteHealth{Status: "degraded", ConsecutiveFailures: int(failures)}
	default:
		return models.RemoteHealth{Status: "down", ConsecutiveFailures: int(failures)}
	}
}
