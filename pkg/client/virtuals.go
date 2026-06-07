package client

import (
	"context"
	"fmt"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (c *Client) ListVirtuals(ctx context.Context) ([]models.Virtual, error) {
	var virtuals []models.Virtual
	err := c.get(ctx, "/api/v2/virtuals", &virtuals)
	return virtuals, err
}

func (c *Client) GetVirtual(ctx context.Context, name string) (*models.Virtual, error) {
	var virt models.Virtual
	err := c.get(ctx, fmt.Sprintf("/api/v2/virtuals/%s", name), &virt)
	return &virt, err
}

func (c *Client) CreateVirtual(ctx context.Context, v *models.Virtual) error {
	return c.post(ctx, "/api/v2/virtuals", v, v)
}

func (c *Client) UpdateVirtual(ctx context.Context, v *models.Virtual) error {
	return c.put(ctx, fmt.Sprintf("/api/v2/virtuals/%s", v.Name), v, v)
}

func (c *Client) DeleteVirtual(ctx context.Context, name string) error {
	return c.delete(ctx, fmt.Sprintf("/api/v2/virtuals/%s", name))
}
