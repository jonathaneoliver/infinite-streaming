# Label facets vs. whole-tuple tokens

How to encode a multi-dimensional condition in the `labels[]` vocabulary
(`<severity>=<event>`, synth-marked `*`). Read before adding a label that
carries more than one dimension.

## The axes of a network failure

A failed `network_requests` row is one event seen along independent axes:

| axis | values |
|---|---|
| kind | `segment` · `manifest` · `master_manifest` |
| transport-cause | `client_disconnect` · `socket` · `active_timeout` · `idle_timeout` · `transport` |
| outcome | `incomplete` · `timeout` · `other` · `http_4xx` · `http_5xx` |

These are emitted as **separate facet labels** (`*segment_failure`,
`*transport_disconnect`, `fault_incomplete`, …) so each axis stays
independently queryable via `has(labels, …)`, carries its own severity, and
grows the vocabulary linearly instead of as a cross-product.

## The rule

**Decompose a condition into per-axis facet labels only on surfaces where each
axis is single-valued per row.** A `network_requests` row is exactly one HTTP
request, so kind/cause/outcome are single-valued — the facets reconstruct one
unambiguous tuple.

**On rows that can carry several incidents at once** (a `session_events`
heartbeat snapshot, a `play_end` rollup) keep labels as **pre-fused
whole-tuple tokens** (`qoe_stall_severe_midplay`). A flat *set* of per-axis
facets cannot say which cause pairs with which kind when two incidents share a
row.

**Never** combine: decomposed facets **+** a multi-incident row **+** no
correlation id. That's the one encoding that loses information. If you ever
must decompose a multi-incident surface, bind each tuple with an instance id
(`i1.kind=…`, `i2.kind=…`) — or just don't decompose it.

## Identity: derive, don't fuse

When you want a single nameable identifier for a decomposed failure (to read,
dedup, or `GROUP BY`), **derive** a canonical whole-tuple signature from the
facets — do not fuse the axes into the stored label *name*.

- Network surface (#892): the forwarder emits `*net_failure:<kind>/<cause>/<outcome>`
  alongside the facets, at their worst severity. `netFailureSignature()` in
  `analytics/go-forwarder/labels.go` mirrors the facet logic so they never
  disagree. The dashboard renders it as one collapsed chip
  (`collapseNetFailureLabels` / `humanizeNetFailure` in
  `content/dashboard-v3/src/lib/labelGlossary.ts`) with the facets on hover.

Normalize for storage/query (facets); denormalize for identity/eyes (signature).

## Rejected alternatives (and when to reconsider)

- **Fused enum names** (`segment_incomplete_transport_disconnect`): kills
  per-axis filtering + per-facet severity, explodes the vocabulary. Never.
- **Axis-keyed labels** (`net.kind=segment`): clean greenfield model, but a
  breaking rename across forwarder/CH/glossary/harness/dashboard. Reserve for a
  ground-up label-grammar revision.
- **Correlation/instance ids**: only needed if a multi-incident surface is ever
  decomposed. Today none are.
- **A stored `failure_signature` column**: unnecessary — the signature-as-label
  already gives `has()` / `GROUP BY`. Add only if a label lookup proves too slow.
