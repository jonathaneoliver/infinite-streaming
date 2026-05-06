//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// pingSocket is a single shared *icmp.PacketConn shared across all
// per-session pingers. A receiver goroutine demuxes Echo Replies by
// (id, seq) into per-call reply channels — so one global socket
// handles arbitrarily many concurrent ping calls without the lock-
// hold-during-recv problem of a serial sender.
//
// The container runs `privileged: true` (see docker-compose.yml),
// so raw IPPROTO_ICMP works; we don't need the unprivileged datagram
// flavour or the ping_group_range sysctl dance.
type pingSocket struct {
	conn      *icmp.PacketConn
	pid       int
	seq       uint32 // atomic
	pendingMu sync.Mutex
	pending   map[uint32]chan time.Time
}

func newPingSocket() (*pingSocket, error) {
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, err
	}
	// IP_TOS = 0x10 (Min Delay) maps to TC_PRIO_INTERACTIVE under the
	// kernel's default rt_tos2priority table, which the per-port
	// `prio` qdisc installed by UpdateNetem routes to band 0 — the
	// high-priority lane that jumps bulk segment data queued in
	// band 1. Issue #404. SetTOS sets the socket-level IP_TOS option
	// so all subsequent sends inherit it; no per-packet cmsg dance.
	if pc := c.IPv4PacketConn(); pc != nil {
		_ = pc.SetTOS(0x10)
	}
	s := &pingSocket{
		conn:    c,
		pid:     os.Getpid() & 0xffff,
		pending: map[uint32]chan time.Time{},
	}
	go s.recvLoop()
	return s, nil
}

func (s *pingSocket) recvLoop() {
	buf := make([]byte, 1500)
	for {
		n, _, err := s.conn.ReadFrom(buf)
		if err != nil {
			// Socket closed or fatal — bail out; next ping attempt
			// will surface the error to the sampler which writes 0.
			return
		}
		replyAt := time.Now()
		msg, perr := icmp.ParseMessage(int(ipv4.ICMPTypeEchoReply.Protocol()), buf[:n])
		if perr != nil {
			continue
		}
		if msg.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := msg.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		key := pingKey(uint16(echo.ID), uint16(echo.Seq))
		s.pendingMu.Lock()
		ch, ok := s.pending[key]
		if ok {
			delete(s.pending, key)
		}
		s.pendingMu.Unlock()
		if ok {
			select {
			case ch <- replyAt:
			default:
			}
		}
	}
}

func pingKey(id, seq uint16) uint32 {
	return (uint32(id) << 16) | uint32(seq)
}

// ping issues one ICMP Echo Request to addr and returns the round-
// trip time in microseconds. timeout caps how long we wait for the
// matching Echo Reply.
func (s *pingSocket) ping(addr string, timeout time.Duration) (uint32, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return 0, errors.New("invalid ip")
	}
	v4 := ip.To4()
	if v4 == nil {
		// IPv6 not currently supported — player_ip is observed at the
		// HTTP request layer where the dashboard's testing flow uses
		// IPv4 throughout. Failing fast here is fine; the chart shows
		// a gap.
		return 0, errors.New("ipv6 not supported")
	}
	seq := uint16(atomic.AddUint32(&s.seq, 1))
	body := &icmp.Echo{
		ID:   s.pid,
		Seq:  int(seq),
		Data: []byte("ism-rtt-401"),
	}
	msg := &icmp.Message{Type: ipv4.ICMPTypeEcho, Code: 0, Body: body}
	payload, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	key := pingKey(uint16(s.pid), seq)
	ch := make(chan time.Time, 1)
	s.pendingMu.Lock()
	s.pending[key] = ch
	s.pendingMu.Unlock()
	cleanup := func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	}
	sentAt := time.Now()
	if _, err := s.conn.WriteTo(payload, &net.IPAddr{IP: v4}); err != nil {
		cleanup()
		return 0, err
	}
	select {
	case replyAt := <-ch:
		return uint32(replyAt.Sub(sentAt).Microseconds()), nil
	case <-time.After(timeout):
		cleanup()
		return 0, errors.New("ping timeout")
	}
}
