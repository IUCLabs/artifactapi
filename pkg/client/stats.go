package client

import (
	"context"
	"fmt"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (c *Client) Stats(ctx context.Context) (*models.OverviewStats, error) {
	var stats models.OverviewStats
	err := c.get(ctx, "/api/v2/stats", &stats)
	return &stats, err
}

func (c *Client) Health(ctx context.Context) (*models.RemoteHealth, error) {
	var health models.RemoteHealth
	err := c.get(ctx, "/api/v2/health", &health)
	return &health, err
}

func (c *Client) ListObjects(ctx context.Context, remote string, page, perPage int) ([]models.Artifact, error) {
	var artifacts []models.Artifact
	err := c.get(ctx, fmt.Sprintf("/api/v2/remotes/%s/objects?page=%d&per_page=%d", remote, page, perPage), &artifacts)
	return artifacts, err
}

func (c *Client) EvictObject(ctx context.Context, remote, path string) error {
	return c.delete(ctx, fmt.Sprintf("/api/v2/remotes/%s/objects/%s", remote, path))
}
