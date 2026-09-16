//go:build !linux

package main

import "net"

// readDeliveryRate on non-Linux platforms is a no-op stub so the dev
// build (typically macOS) keeps compiling. tcpi_delivery_rate and its
// app-limited flag are Linux-specific TCP_INFO fields; the production
// go-proxy only runs in the Linux container. Returning 0/false leaves
// delivery_rate_mbps unset and app_limited false.
func readDeliveryRate(c *net.TCPConn) (bps uint64, appLimited bool, err error) {
	return 0, false, nil
}
