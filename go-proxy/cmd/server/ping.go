package main

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// Path-ping signal (issue #404). Sampled out-of-band of the streaming
// TCP connection — ICMP echo from go-proxy → player_ip at 1 Hz so the
// chart can show "what's the underlying path latency right now"
// independently of the application's own queue contribution. The
// TCP_INFO RTT shipped in #401 inflates with throttle because it's
// always self-loaded; ICMP packets are tiny and don't fight for
// bandwidth with the segment bytes, so they reflect the path's
// latency floor in real time, not just at moments when the kernel
// happened to capture an empty queue.

// sessionGetOrCreatePingRTT installs a per-session *atomic.Uint32
// (microseconds) holder under SessionData["_pingRTTUs"] if missing.
// Same survival-across-clone trick as the TCP_INFO conn / window
// holders — cloneInterface's default arm passes unknown pointer
// types through, so the holder created in handleProxy persists
// through every subsequent snapshot.
func sessionGetOrCreatePingRTT(s SessionData) *atomic.Uint32 {
	if s == nil {
		return nil
	}
	if h, ok := s["_pingRTTUs"].(*atomic.Uint32); ok && h != nil {
		return h
	}
	h := &atomic.Uint32{}
	s["_pingRTTUs"] = h
	return h
}

func sessionLoadPingRTT(s SessionData) *atomic.Uint32 {
	if s == nil {
		return nil
	}
	if h, ok := s["_pingRTTUs"].(*atomic.Uint32); ok {
		return h
	}
	return nil
}

// startPathPingSampler runs a 1 Hz ticker that pings each active
// session's player_ip via ICMP echo and stamps the round-trip time
// (microseconds) into the per-session atomic holder. Drained by
// drainSessionRTT and surfaced as `client_path_ping_rtt_ms` on the
// snapshot. 1 Hz matches the snapshot emit cadence — no folding
// needed; the chart line gets one fresh sample per tick.
//
// Failures (ICMP filtered, host unreachable, malformed IP) write 0
// so the chart shows a gap rather than dragging stale values forward.
// On non-Linux builds newPingSocket returns (nil, nil) and we log a
// disabled message; the rest of the proxy keeps running.
func (a *App) startPathPingSampler(ctx context.Context) {
	sock, err := newPingSocket()
	if err != nil {
		log.Printf("path ping sampler disabled: %v", err)
		return
	}
	if sock == nil {
		log.Printf("path ping sampler disabled: ICMP not supported on this build")
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snap := a.sessionsSnap.Load()
				if snap == nil {
					continue
				}
				for _, session := range *snap {
					holder := sessionLoadPingRTT(session)
					if holder == nil {
						continue
					}
					ip := strings.TrimSpace(getString(session, "player_ip"))
					if ip == "" {
						holder.Store(0)
						continue
					}
					rttUs, perr := sock.ping(ip, 200*time.Millisecond)
					if perr != nil {
						holder.Store(0)
						continue
					}
					holder.Store(rttUs)
				}
			}
		}
	}()
	log.Printf("path ping sampler started (1Hz)")
}

// stampSessionPathPing reads the latest per-session ping holder and
// stamps client_path_ping_rtt_ms onto the session map. Called from
// normalizeSessionsForResponse alongside drainSessionRTT.
func stampSessionPathPing(session SessionData) {
	holder := sessionLoadPingRTT(session)
	if holder == nil {
		return
	}
	rttUs := holder.Load()
	if rttUs == 0 {
		// No fresh ping this tick (timeout, ICMP filtered, no IP).
		// Delete rather than emitting 0 so the chart renders a gap.
		delete(session, "client_path_ping_rtt_ms")
		return
	}
	session["client_path_ping_rtt_ms"] = float64(rttUs) / 1000.0
}
