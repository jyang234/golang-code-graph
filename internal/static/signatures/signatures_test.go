package signatures_test

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/signatures"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func TestMethodSignatureIncludesReceiver(t *testing.T) {
	prog, err := statictest.Build()
	if err != nil {
		t.Fatal(err)
	}
	fn := statictest.FindFuncExact(prog, "(*example.com/loansvc/internal/handler.App).Create")
	if fn == nil {
		t.Fatal("handler.App.Create not found")
	}
	sig := signatures.Of(fn)
	if !strings.Contains(sig, "App") {
		t.Errorf("signature omits receiver: %q", sig)
	}
	if !strings.Contains(sig, "ResponseWriter") || !strings.Contains(sig, "Request") {
		t.Errorf("signature omits parameters: %q", sig)
	}
}

func TestGenericSignatureIncludesTypeParams(t *testing.T) {
	prog, err := statictest.Build()
	if err != nil {
		t.Fatal(err)
	}
	// The generic origin, not its instantiation codec.Decode[…Application] —
	// FindFunc would (correctly) reject the bare substring as ambiguous.
	fn := statictest.FindFuncExact(prog, "example.com/loansvc/internal/codec.Decode")
	if fn == nil {
		t.Fatal("codec.Decode not found")
	}
	sig := signatures.Of(fn)
	if !strings.Contains(sig, "[T any]") {
		t.Errorf("generic signature omits type parameters: %q", sig)
	}
	if !strings.Contains(sig, "Reader") {
		t.Errorf("generic signature omits parameters: %q", sig)
	}
}
