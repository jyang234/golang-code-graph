// Package blobstore is a thin object-storage client, classified as the `blob`
// boundary kind (see .flowmap.yaml). It stands in for an S3/GCS SDK: its operations
// are method names the labeler reads as the boundary op, with no readable
// peer/op/target triple and no sound write/read verb.
package blobstore

import "context"

// Client writes and reads objects.
type Client struct{}

// New returns a Client.
func New() *Client { return &Client{} }

// PutObject stores an object. Never executed under static analysis.
func (c *Client) PutObject(ctx context.Context, key string) error {
	_, _ = ctx, key
	return nil
}

// GetObject fetches an object.
func (c *Client) GetObject(ctx context.Context, key string) error {
	_, _ = ctx, key
	return nil
}
