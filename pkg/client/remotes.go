package client

import (
	"context"
	"fmt"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (c *Client) ListRemotes(ctx context.Context) ([]models.Remote, error) {
	var remotes []models.Remote
	err := c.get(ctx, "/api/v2/remotes", &remotes)
	return remotes, err
}

func (c *Client) GetRemote(ctx context.Context, name string) (*models.Remote, error) {
	var remote models.Remote
	err := c.get(ctx, fmt.Sprintf("/api/v2/remotes/%s", name), &remote)
	return &remote, err
}

func (c *Client) CreateRemote(ctx context.Context, r *models.Remote) error {
	return c.post(ctx, "/api/v2/remotes", r, r)
}

func (c *Client) UpdateRemote(ctx context.Context, r *models.Remote) error {
	return c.put(ctx, fmt.Sprintf("/api/v2/remotes/%s", r.Name), r, r)
}

func (c *Client) DeleteRemote(ctx context.Context, name string) error {
	return c.delete(ctx, fmt.Sprintf("/api/v2/remotes/%s", name))
}
