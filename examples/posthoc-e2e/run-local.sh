#!/usr/bin/env bash
# run-local.sh — the post-hoc e2e reference, end to end, without Docker.
#
# Starts a real OTel Collector + the OTLP-exporting loansvc stand-in, drives one
# flowmap-tagged request, then runs `flowmap behavior ingest` on the collector's
# file output. This is the known-good chain to diff your own setup against.
#
# Requires: `otelcol-contrib` on PATH (https://github.com/open-telemetry/
# opentelemetry-collector-releases/releases) and the Go toolchain.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
WORK="$(mktemp -d)"
export FLOWMAP_TRACE_FILE="$WORK/traces.json"

cleanup() { kill -TERM "${SVC:-}" "${COL:-}" 2>/dev/null || true; }
trap cleanup EXIT

echo "› starting collector (file → $FLOWMAP_TRACE_FILE)"
otelcol-contrib --config "$HERE/otel-collector.yaml" >"$WORK/collector.log" 2>&1 &
COL=$!

echo "› building + starting loansvc-otlp (:8080 → OTLP localhost:4318)"
( cd "$HERE/service" && GOWORK=off go build -o "$WORK/svc" . )
"$WORK/svc" >"$WORK/service.log" 2>&1 &   # defaults to OTLP http://localhost:4318
SVC=$!
sleep 3

echo "› driving the flow (withFlow sets the flowmap.flow baggage member)"
curl -s -o /dev/null -w "  POST /loan-application → %{http_code}\n" \
  -X POST -H 'baggage: flowmap.flow=loan-application' localhost:8080/loan-application

echo "› waiting for batch export + tail-sampling decision_wait, then draining"
sleep 4
kill -TERM "$SVC" 2>/dev/null || true; sleep 2   # flush the SDK batcher
kill -TERM "$COL" 2>/dev/null || true; sleep 2   # flush the collector file exporter
SVC=""; COL=""

echo "› flowmap behavior ingest:"
( cd "$ROOT" && go run ./cmd/flowmap behavior ingest "$FLOWMAP_TRACE_FILE" )
echo
echo "captured trace file: $FLOWMAP_TRACE_FILE"
echo "(to gate: rerun with --flows-dir <dir> --update to snapshot, then without --update to enforce)"
