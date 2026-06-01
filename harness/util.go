package harness

import (
	"bytes"
	"io"

	"github.com/jyang234/golang-code-graph/internal/canon/url"
)

// templatePath reduces a request path to its route template (/score/8412 →
// /score/{id}), reusing the canonicalizer's URL templating so the server span's
// http.route matches what canon would derive.
func templatePath(p string) string { return url.Template(p) }

// bytesReader returns an io.Reader over body, or nil for an empty body so
// httptest.NewRequest produces a nil-bodied request.
func bytesReader(body []byte) io.Reader {
	if len(body) == 0 {
		return nil
	}
	return bytes.NewReader(body)
}
