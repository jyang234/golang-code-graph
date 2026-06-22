// Package rpcclient is a thin non-HTTP RPC peer client, classified as the `rpc`
// boundary kind (see .flowmap.yaml). Its operations are method names (the generated
// RPC client surface), with no readable peer/method/route triple.
package rpcclient

import "context"

// Client calls a non-HTTP RPC peer.
type Client struct{}

// New returns a Client.
func New() *Client { return &Client{} }

// Charge invokes the peer's Charge RPC. Never executed under static analysis.
func (c *Client) Charge(ctx context.Context, id string) error {
	_, _ = ctx, id
	return nil
}
