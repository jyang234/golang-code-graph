// Command blobsvc is the method-named outbound effect-kind fixture (object storage,
// cache, non-HTTP RPC). The HTTP route uploadHandler is a root, so its calls appear
// as typed boundary edges (`boundary:blob`/`boundary:cache`/`boundary:rpc`), as
// external dependencies of those kinds in the gated contract, and — because their
// write-ness is unreadable — as budget-unenforceable disclosures.
package main

import (
	"net/http"

	"example.com/blobsvc/blobstore"
	"example.com/blobsvc/cacheclient"
	"example.com/blobsvc/rpcclient"
)

var (
	store = blobstore.New()
	cache = cacheclient.New()
	peer  = rpcclient.New()
)

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = store.PutObject(ctx, "k") // boundary:blob PutObject
	_ = store.GetObject(ctx, "k") // boundary:blob GetObject
	_ = cache.Get(ctx, "k")       // boundary:cache Get
	_ = cache.Set(ctx, "k")       // boundary:cache Set
	_ = peer.Charge(ctx, "id")    // boundary:rpc Charge
}

func main() {
	http.HandleFunc("/upload", uploadHandler)
	_ = http.ListenAndServe(":8080", nil)
}
