// withFlow tags every request made inside `fn` with a flowmap.flow baggage
// member, so the collector can tail-sample the trace and flowmap can name the
// golden by <slug>. This is the entire per-test authoring cost.
//
// It assumes your services run a `baggagecopy` span processor that promotes the
// flowmap.flow baggage member onto spans (see docs/integration/README.md) —
// baggage alone does not reach the collector.
//
// No flowmap client library in TypeScript: this only sets the standard W3C
// `baggage` header. If you already propagate a Correlation-Id, leave it as-is;
// flowmap can group on it as an alternate key.

import { context, baggage as otelBaggage, propagation } from "@opentelemetry/api";

/**
 * Run `fn` with `flowmap.flow=<slug>` attached to the active OTel context, so
 * outbound requests carry it as W3C baggage.
 *
 * @param slug  stable flow id; becomes the golden file name.
 * @param fn    the body of the flow (your existing Playwright steps).
 */
export async function withFlow<T>(slug: string, fn: () => Promise<T>): Promise<T> {
  const bag = (otelBaggage.getActiveBaggage() ?? propagation.createBaggage()).setEntry(
    "flowmap.flow",
    { value: slug },
  );
  return context.with(propagation.setBaggage(context.active(), bag), fn);
}

// --- usage ---
//
// test("loan publish fan-out", async ({ request }) => {
//   await withFlow("publish-fanout", async () => {
//     await request.post("/loan-application", { data: { amount: 5000 } });
//     // … assert your normal e2e expectations …
//   });
// });
//
// If your HTTP client does not auto-inject baggage from the active context,
// inject it explicitly per request:
//
//   const headers: Record<string, string> = {};
//   propagation.inject(context.active(), headers);   // sets `baggage`/`traceparent`
//   await request.post(url, { headers, data });
