// Package cacheclient is a thin cache client, classified as the `cache` boundary
// kind (see .flowmap.yaml). Like object storage, its operations are method names
// the labeler reads as the op, with no sound write/read verb.
package cacheclient

import "context"

// Client reads and writes cache entries.
type Client struct{}

// New returns a Client.
func New() *Client { return &Client{} }

// Get fetches a cache entry. Never executed under static analysis.
func (c *Client) Get(ctx context.Context, key string) error {
	_, _ = ctx, key
	return nil
}

// Set writes a cache entry.
func (c *Client) Set(ctx context.Context, key string) error {
	_, _ = ctx, key
	return nil
}
