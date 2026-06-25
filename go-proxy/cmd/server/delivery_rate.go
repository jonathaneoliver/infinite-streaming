package main

import "net"

// stampDeliveryRate samples the kernel's tcpi_delivery_rate on the
// client connection and records it (Mbps) on the network-log entry.
//
// This is the honest throughput cross-check for the bytes_out /
// transfer_ms figure, which over-reports badly whenever a transfer fits
// inside the socket send buffer + tc qdisc backlog (it then times only
// the memcpy into the kernel, not the shaped wire delivery).
//
// Connection-level, not per-stream: tcpi_delivery_rate is a property of
// the TCP socket, so under HTTP/2 (where audio + video segments share
// one connection) it reflects the whole socket's recent delivery rate,
// not this one segment in isolation. That's still the correct
// shaped-rate signal — it just can't be attributed to a single stream.
//
// Best-effort: a read error (non-Linux build, torn-down conn) leaves
// delivery_rate_mbps unset rather than failing the request. Mbps here is
// decimal (bytes/s × 8 ÷ 1e6).
func stampDeliveryRate(conn *net.TCPConn, entry *NetworkLogEntry) {
	if conn == nil || entry == nil {
		return
	}
	if bps, err := readDeliveryRateBps(conn); err == nil && bps > 0 {
		entry.DeliveryRateMbps = float64(bps) * 8 / 1e6
	}
}
