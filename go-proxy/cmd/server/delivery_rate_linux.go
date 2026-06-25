//go:build linux

package main

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// readDeliveryRateBps issues a single getsockopt(TCP_INFO) and returns
// the kernel's tcpi_delivery_rate (bytes/sec) — its own estimate of the
// rate bytes are leaving on the wire for this connection. Same
// SyscallConn().Control path as readTCPInfo; ~1 µs per call.
//
// Why this exists: the bytes_out/transfer_ms figure in the network log
// times only the proxy's write+flush, which returns once the kernel
// accepts the bytes into the socket send buffer — NOT when they reach
// the client. tc HTB shaping drains the qdisc *below* the socket, so a
// sub-buffer segment (~50–140 KB) is absorbed instantly and reports
// 1000s of Mbps even while the wire is capped near the video bitrate.
// tcpi_delivery_rate reflects the actual drained rate instead.
func readDeliveryRateBps(c *net.TCPConn) (uint64, error) {
	if c == nil {
		return 0, errors.New("nil tcp conn")
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var rate uint64
	var sysErr error
	ctlErr := rc.Control(func(fd uintptr) {
		var info *unix.TCPInfo
		info, sysErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if sysErr == nil && info != nil {
			rate = info.Delivery_rate
		}
	})
	if ctlErr != nil {
		return 0, ctlErr
	}
	if sysErr != nil {
		return 0, sysErr
	}
	return rate, nil
}
