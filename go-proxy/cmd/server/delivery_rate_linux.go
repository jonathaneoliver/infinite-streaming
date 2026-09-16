//go:build linux

package main

import (
	"errors"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// readDeliveryRate issues a single getsockopt(TCP_INFO) and returns the
// kernel's tcpi_delivery_rate (bytes/sec) — its own estimate of the rate
// bytes are leaving on the wire for this connection — together with the
// tcpi_delivery_rate_app_limited flag. ~1 µs per call.
//
// Why this exists: the bytes_out/transfer_ms figure in the network log
// times only the proxy's write+flush, which returns once the kernel
// accepts the bytes into the socket send buffer — NOT when they reach
// the client. tc HTB shaping drains the qdisc *below* the socket, so a
// sub-buffer segment (~50–140 KB) is absorbed instantly and reports
// 1000s of Mbps even while the wire is capped near the video bitrate.
// tcpi_delivery_rate reflects the actual drained rate instead.
//
// The app-limited flag says whether that rate sample is trustworthy:
// when the sender ran out of data to push (the common case for a small
// HLS segment), the kernel could not observe the link at full tilt, so
// the rate is noisy — it can read far below the cap (starved) OR above
// it (a token-bucket burst caught in a short window). A network-limited
// sample (app_limited == false) is the one that converges on the cap.
//
// Raw getsockopt: golang.org/x/sys/unix.TCPInfo silently drops the
// bitfield byte that carries this flag — it falls in struct alignment
// padding between Options and Rto and has no Go field — so we read the
// raw TCP_INFO buffer and pull the bit ourselves. In the kernel struct,
// byte 7 is `tcpi_delivery_rate_app_limited:1, tcpi_fastopen_client_fail:2`,
// an ABI-stable offset among the oldest tcp_info fields; bit 0 is the
// app-limited flag. Delivery_rate is read through the struct overlay,
// whose exposed fields keep the kernel's byte offsets.
func readDeliveryRate(c *net.TCPConn) (bps uint64, appLimited bool, err error) {
	if c == nil {
		return 0, false, errors.New("nil tcp conn")
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, false, err
	}
	var rate uint64
	var limited bool
	var sysErr error
	ctlErr := rc.Control(func(fd uintptr) {
		var buf [unix.SizeofTCPInfo]byte
		vallen := uint32(len(buf))
		_, _, errno := unix.Syscall6(
			unix.SYS_GETSOCKOPT,
			fd,
			uintptr(unix.IPPROTO_TCP),
			uintptr(unix.TCP_INFO),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&vallen)),
			0,
		)
		if errno != 0 {
			sysErr = errno
			return
		}
		info := (*unix.TCPInfo)(unsafe.Pointer(&buf[0]))
		rate = info.Delivery_rate
		if vallen > 7 {
			limited = buf[7]&0x01 != 0
		}
	})
	if ctlErr != nil {
		return 0, false, ctlErr
	}
	if sysErr != nil {
		return 0, false, sysErr
	}
	return rate, limited, nil
}
