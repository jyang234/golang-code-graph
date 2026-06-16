#!/usr/bin/env bash
# Regenerate the committed groundwork graph goldens from the fixture services.
#
# These goldens are the *input* to groundwork's tests: groundwork consumes
# flowmap's graph JSON, so its fixtures are graphs, not source. We commit the
# source modules too (for provenance and so a flowmap change that shifts the
# graph shape is caught here, the same discipline flowmap uses for its boundary
# contract). Run from the repo root after changing a fixture or the graph schema:
#
#     go run ./cmd/flowmap >/dev/null   # ensure it builds
#     testdata/groundwork/regen.sh
#     git diff testdata/groundwork/goldens   # review the delta
#
# Section counts are pinned in goldens/manifest.json, which this script
# deliberately does NOT rewrite: if a regen changes a section's size, the
# manifest test fails until you update the manifest by hand in the same
# commit — the ratchet that keeps a regen from laundering an analyzer
# regression into the goldens (it happened once: see
# TestGoldenSectionManifest).
set -euo pipefail
cd "$(dirname "$0")/../.."

flowmap() { go run ./cmd/flowmap "$@"; }

# flowmap stamps the build that produced the graph into the header (the `tool`
# producer field, R11). Strip it from the committed goldens: like the --stamp code
# identity, the producing-tool version is provenance that must NOT pollute a golden,
# or the fixtures would churn every time a different flowmap build regenerated them.
# Delete the key JSON-aware (pop, not a line-grep) so the strip cannot silently miss
# the field if canonjson's formatting (indent width, key position) ever shifts, then
# re-emit in canonjson's exact shape — 2-space indent, non-ASCII kept literal
# (ensure_ascii=False), trailing newline — so the golden stays byte-identical to
# `flowmap graph` output minus the one field. The round-trip preserves key order
# (json.load/dump keep insertion order = canonjson's struct order).
strip_tool() {
	python3 - "$1" <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    g = json.load(f)
g.pop("tool", None)
with open(path, "w") as f:
    json.dump(g, f, indent=2, ensure_ascii=False)
    f.write("\n")
PY
}

for svc in layeredsvc blindsvc obligsvc; do
	dir="testdata/groundwork/$svc"
	out="testdata/groundwork/goldens/$svc.graph.json"
	flowmap graph "$dir" >"$out"
	strip_tool "$out"
	echo "wrote $out"
done

# loansvc is flowmap's richer fixture; we keep a copy of its graph as a groundwork
# golden purely so the effect-label contract test (review) sees the full label
# vocabulary the classifiers depend on — db read/write, bus publish/consume, and
# outbound GET/POST. If flowmap's label format ever changes, regenerating this is
# what makes the contract test fail instead of silently misclassifying effects.
flowmap graph testdata/fixtures/loansvc >testdata/groundwork/goldens/loansvc.graph.json
strip_tool testdata/groundwork/goldens/loansvc.graph.json
echo "wrote testdata/groundwork/goldens/loansvc.graph.json"

# Boundary contracts for the `diff` demo. flowmap boundary writes in-place, so we
# generate, copy to the goldens dir, and drop the in-fixture file. The branch
# contract is the base with the PUT route removed (breaking), a /healthz route
# added (additive), and a new outbound dependency (informational).
go run ./cmd/flowmap boundary testdata/groundwork/layeredsvc >/dev/null
cp testdata/groundwork/layeredsvc/.flowmap/boundary-contract.json testdata/groundwork/goldens/layeredsvc.contract.json
rm -rf testdata/groundwork/layeredsvc/.flowmap
python3 - <<'PY'
import json
c = json.load(open("testdata/groundwork/goldens/layeredsvc.contract.json"))
c["entrypoints"]["http"] = [e for e in c["entrypoints"]["http"] if e["method"] != "PUT"]
c["entrypoints"]["http"].append({"method": "GET", "route": "/healthz", "tier": 2})
c["entrypoints"]["http"].sort(key=lambda e: (e["method"], e["route"]))
c["external_dependencies"].append({"peer": "audit-svc", "kind": "http", "ops": ["POST /events"], "tier": 1})
json.dump(c, open("testdata/groundwork/goldens/layeredsvc.branch.contract.json", "w"), indent=2)
open("testdata/groundwork/goldens/layeredsvc.branch.contract.json", "a").write("\n")
PY
echo "wrote testdata/groundwork/goldens/layeredsvc{,.branch}.contract.json"

# Branch goldens for the review demo. groundwork's `review` compares a base graph
# to a branch graph; in CI both come from flowmap run on the respective code. Here
# we synthesize the branch graphs by applying one documented feature delta to the
# real layeredsvc base — "add a GetUserFast read endpoint" — wired two ways:
#
#   branch-good: GetUserFast → app.GetProfile   (handler → app: correct)
#   branch-skip: GetUserFast → store.SelectUser (handler → store: skips the app layer)
#
# Same feature, same description; the only difference is one edge. That is exactly
# what flowmap would emit for the two source variants, and it is what makes the
# review verdict (STRUCTURALLY-CLEAR vs BLOCK) a property of the code, not the prose.
python3 - <<'PY'
import json

base = json.load(open("testdata/groundwork/goldens/layeredsvc.graph.json"))
H = "(*example.com/layeredsvc/internal/handler.Server)"
fast = {
    "fqn": f"{H}.GetUserFast",
    "sig": "func (*handler.Server).GetUserFast(w http.ResponseWriter, r *http.Request)",
    "tier": 1,
}

def branch(target, out):
    g = json.loads(json.dumps(base))
    g["nodes"].append(dict(fast))
    g["nodes"].sort(key=lambda n: n["fqn"])
    g["edges"].append({"from": f"{H}.GetUserFast", "to": target, "tier": 2})
    g["edges"].sort(key=lambda e: (e["from"], e["to"], e["tier"]))
    json.dump(g, open(out, "w"), indent=2)
    open(out, "a").write("\n")
    print("wrote", out)

branch("(*example.com/layeredsvc/internal/app.Service).GetProfile",
       "testdata/groundwork/goldens/layeredsvc.branch-good.graph.json")
branch("(*example.com/layeredsvc/internal/store.Store).SelectUser",
       "testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json")
PY

# A committed review artifact (the canonical JSON form), for the verify-artifact
# example and CLI test. It carries the digest, so it is regenerated here whenever
# the artifact shape or digest computation changes.
go run ./cmd/groundwork review \
	testdata/groundwork/policies/layeredsvc.json \
	testdata/groundwork/goldens/layeredsvc.graph.json \
	testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json \
	--json >testdata/groundwork/goldens/layeredsvc.branch-skip.artifact.json || true
echo "wrote testdata/groundwork/goldens/layeredsvc.branch-skip.artifact.json"

