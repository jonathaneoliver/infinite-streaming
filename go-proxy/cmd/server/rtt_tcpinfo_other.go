//go:build !linux

package main

import "net"

// readTCPInfo on non-Linux platforms is a no-op stub so the dev build
// (typically macOS) keeps compiling. The whole RTT chart is empty in
// that mode — TCP_INFO is a Linux-specific socket option and the
// production go-proxy only runs in the Linux container anyway.
func readTCPInfo(c *net.TCPConn) (tcpInfoSnapshot, error) {
	return tcpInfoSnapshot{}, nil
}
