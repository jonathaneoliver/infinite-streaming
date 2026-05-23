// Package plays is the typed domain layer over ClickHouse for play-
// centric read operations.
//
// The HTTP handlers in the forwarder's main package are thin shims:
// parse the request, call into this package, render the response. The
// chat backend's typed tools (see #497) call the same functions
// directly, in-process, with no loopback HTTP / JSON tax.
//
// What lives here: anything that has a typed parameter / typed return
// shape — list/get/filter operations, classification mutations,
// labels-aware aggregations. Anything that's intrinsically streaming
// (NDJSON snapshot replay, the v2 timeseries SSE multiplex, ZIP bundle
// streaming) stays in the main package — those don't fit a "function
// returns a slice" model.
//
// What does NOT live here: SSE event hubs, ring buffers, write-side
// batchers, label classifiers (those run at ingest time, not query
// time). They're all in main.
package plays
