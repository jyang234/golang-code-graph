// Package client holds the fixture's outbound calls to peer services. Every
// external call goes through (*Client).Call with constant peer, method, and route
// arguments — the recognized outbound seam named under .flowmap.yaml's
// classify.http. That constancy is what lets the static extractor name each
// external dependency (credit-bureau GET /score/{id}, payment-gw POST
// /charge/{id}); a dynamically-built target would instead surface as a blind spot.
package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// tracer is fetched per span (never cached at package init): the OTel
// global binds a cached delegating tracer to the first provider installed,
// which would route a second in-process test's spans to the first test's
// recorder. Fetching per span always resolves the current provider.
func tracer() trace.Tracer { return otel.Tracer("loansvc") }

// Client performs outbound HTTP requests to peer services.
type Client struct {
	hc *http.Client
}

// New returns a Client using the default HTTP client.
func New() *Client { return &Client{hc: http.DefaultClient} }

// NewWithClient returns a Client over a caller-supplied *http.Client. The
// behavioral harness injects one with a fake transport so outbound calls to peer
// services resolve hermetically; production uses New.
func NewWithClient(hc *http.Client) *Client { return &Client{hc: hc} }

// Call issues method to peer at the route template (e.g. "/score/{id}"), filling
// the template from params. peer, method, and route are passed as constants at
// every call site so the static extractor can record the dependency. The call is
// wrapped in a client-kind span the behavioral pipeline canonicalizes to
// "HTTP <METHOD> <peer> <route>".
func (c *Client) Call(ctx context.Context, peer, method, route string, params ...any) ([]byte, error) {
	ctx, span := tracer().Start(ctx, "http.client", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	span.SetAttributes(
		attribute.String("http.request.method", method),
		attribute.String("peer.service", peer),
		attribute.String("http.route", route),
	)
	url := "http://" + peer + expand(route, params...)
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: status %d", method, peer, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// expand fills a "/a/{x}/b" route template from params, in order. Static analysis
// never runs this; the behavioral harness does.
func expand(route string, params ...any) string {
	out := route
	for _, p := range params {
		i := strings.IndexByte(out, '{')
		j := strings.IndexByte(out, '}')
		if i < 0 || j <= i {
			break
		}
		out = out[:i] + fmt.Sprint(p) + out[j+1:]
	}
	return out
}

// Bureau is the typed client for the credit-bureau peer.
type Bureau struct {
	c *Client
}

// NewBureau returns a Bureau over c.
func NewBureau(c *Client) *Bureau { return &Bureau{c: c} }

// Score fetches an applicant's credit score: GET credit-bureau /score/{id}.
func (b *Bureau) Score(ctx context.Context, id string) (int, error) {
	if _, err := b.c.Call(ctx, "credit-bureau", http.MethodGet, "/score/{id}", id); err != nil {
		return 0, err
	}
	return 720, nil
}

// Gateway is the typed client for the payment-gw peer.
type Gateway struct {
	c *Client
}

// NewGateway returns a Gateway over c.
func NewGateway(c *Client) *Gateway { return &Gateway{c: c} }

// Charge authorizes a disbursement: POST payment-gw /charge/{id}.
func (g *Gateway) Charge(ctx context.Context, id string) error {
	_, err := g.c.Call(ctx, "payment-gw", http.MethodPost, "/charge/{id}", id)
	return err
}
