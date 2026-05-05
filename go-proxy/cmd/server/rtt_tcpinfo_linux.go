//go:build linux

package main

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// readTCPInfo issues a single getsockopt(TCP_INFO) on the given
// connection's socket fd via SyscallConn().Control. Returns the four
// kernel fields we chart (smoothed RTT, jitter, lifetime min RTT,
// retransmit timeout) in microseconds. ~1 µs per call; the 100 ms
// sampler ticks at most 8 sessions × 10 Hz ≈ 80 µs/sec total.
func readTCPInfo(c *net.TCPConn) (tcpInfoSnapshot, error) {
	if c == nil {
		return tcpInfoSnapshot{}, errors.New("nil tcp conn")
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return tcpInfoSnapshot{}, err
	}
	var info *unix.TCPInfo
	var sysErr error
	ctlErr := rc.Control(func(fd uintptr) {
		info, sysErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	})
	if ctlErr != nil {
		return tcpInfoSnapshot{}, ctlErr
	}
	if sysErr != nil {
		return tcpInfoSnapshot{}, sysErr
	}
	if info == nil {
		return tcpInfoSnapshot{}, errors.New("tcp_info nil")
	}
	return tcpInfoSnapshot{
		rttUs:    info.Rtt,
		rttVarUs: info.Rttvar,
		minRTTUs: info.Min_rtt,
		rtoUs:    info.Rto,
	}, nil
}
