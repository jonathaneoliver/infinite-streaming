//go:build !linux

package main

import "net"

// readDeliveryRateBps on non-Linux platforms is a no-op stub so the dev
// build (typically macOS) keeps compiling. tcpi_delivery_rate is a
// Linux-specific TCP_INFO field; the production go-proxy only runs in
// the Linux container. Returning 0 leaves delivery_rate_mbps unset.
func readDeliveryRateBps(c *net.TCPConn) (uint64, error) {
	return 0, nil
}
