// Package redisbus publishes live job progress for SSE consumers.
package redisbus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-redis/redis/v9"
)

type Client struct {
	c *redis.Client
}

func NewClient(url string) *Client {
	opts, err := redis.ParseURL(url)
	if err != nil {
		opts = &redis.Options{Addr: url}
	}
	return &Client{c: redis.NewClient(opts)}
}

func (c *Client) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	return c.c.Close()
}

func (c *Client) PublishProgress(ctx context.Context, jobID int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.c.Publish(ctx, fmt.Sprintf("job:%d:progress", jobID), raw).Err()
}
