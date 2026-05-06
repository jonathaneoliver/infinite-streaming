package main

import (
	"context"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// tcpInfoSnapshot is the platform-agnostic subset of struct tcp_info
// we care about. All values are in microseconds. Populated by
// readTCPInfo (Linux build) or zeroed by the !linux stub.
type tcpInfoSnapshot struct {
	rttUs    uint32 // tcpi_rtt — smoothed RTT (RFC 6298 SRTT)
	rttVarUs uint32 // tcpi_rttvar — smoothed mean deviation (jitter)
	minRTTUs uint32 // tcpi_min_rtt — connection-lifetime min (Linux 4.6+)
	rtoUs    uint32 // tcpi_rto — current retransmit timeout
}

// tcpConnCtxKey is the context key under which we stash the underlying
// *net.TCPConn from http.Server.ConnContext. Required because
// handleProxy otherwise has no way to reach the live TCP connection
// (Hijacker would tear it out of the server, which we don't want).
type tcpConnCtxKey struct{}

// withTCPConnContext is the http.Server.ConnContext callback. Down-
// casts c to *net.TCPConn (true for the stdlib HTTP listener) and
// stamps it on the per-connection context so handleProxy can pull it
// off later via tcpConnFromContext.
func withTCPConnContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*net.TCPConn); ok {
		return context.WithValue(ctx, tcpConnCtxKey{}, tc)
	}
	return ctx
}

func tcpConnFromContext(ctx context.Context) *net.TCPConn {
	if tc, ok := ctx.Value(tcpConnCtxKey{}).(*net.TCPConn); ok {
		return tc
	}
	return nil
}

// sessionStoreTCPConn updates (or lazily creates) the per-session
// *atomic.Pointer[net.TCPConn] holder under SessionData["_lastTCPConn"].
// The same holder pointer is preserved across cloneSession() calls
// (cloneInterface's default arm passes unknown types through), so the
// sampler ticker's reads always see the latest connection without
// allocating a new holder per request.
func sessionStoreTCPConn(s SessionData, c *net.TCPConn) {
	if s == nil || c == nil {
		return
	}
	if existing, ok := s["_lastTCPConn"].(*atomic.Pointer[net.TCPConn]); ok && existing != nil {
		existing.Store(c)
		return
	}
	holder := &atomic.Pointer[net.TCPConn]{}
	holder.Store(c)
	s["_lastTCPConn"] = holder
}

func sessionLoadTCPConn(s SessionData) *net.TCPConn {
	if s == nil {
		return nil
	}
	if h, ok := s["_lastTCPConn"].(*atomic.Pointer[net.TCPConn]); ok && h != nil {
		return h.Load()
	}
	return nil
}

// sessionGetOrCreateRTTWindow returns the per-session RTTWindow,
// allocating one on first call. Same survival-across-clone logic as
// the conn holder above — created once, then carried forward through
// every cloneSession.
func sessionGetOrCreateRTTWindow(s SessionData) *RTTWindow {
	if s == nil {
		return nil
	}
	if w, ok := s["_rttWindow"].(*RTTWindow); ok && w != nil {
		return w
	}
	w := &RTTWindow{}
	s["_rttWindow"] = w
	return w
}

func sessionLoadRTTWindow(s SessionData) *RTTWindow {
	if s == nil {
		return nil
	}
	if w, ok := s["_rttWindow"].(*RTTWindow); ok {
		return w
	}
	return nil
}

// RTTWindow accumulates 100 ms-cadence TCP_INFO samples between
// snapshot emits (1 s heartbeat). drainAndReset folds the window into
// a single rttSample and clears the running aggregates so the next
// 1 s window starts empty.
type RTTWindow struct {
	mu                  sync.Mutex
	sumUs, count        uint64
	maxUs, minUs        uint32
	latestRTOUs         uint32
	latestMinLifetimeUs uint32
	latestVarUs         uint32
	// Last successful drain — replayed when count == 0 (no fresh
	// kernel samples this 1 s window) so the chart line continues
	// across a brief connection gap with a `stale: true` marker
	// rather than dropping to zero.
	lastAvgUs, lastMaxUs, lastMinUs uint32
	haveLastDrain                   bool
}

type rttSample struct {
	avgMs, maxMs, minMs, minLifetimeMs, varMs, rtoMs float32
	hasData                                          bool
	stale                                            bool
}

func (w *RTTWindow) fold(s tcpInfoSnapshot) {
	if w == nil {
		return
	}
	// All-zero snapshots (macOS stub, or a Linux read where the
	// kernel hasn't populated fields yet on a brand-new socket) are
	// no-ops — we don't want to clobber the previous good
	// rto/var/lifetime values with zeros, since the drain path
	// replays them on stale windows.
	if s.rttUs == 0 && s.rtoUs == 0 && s.minRTTUs == 0 && s.rttVarUs == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if s.rttUs > 0 {
		w.sumUs += uint64(s.rttUs)
		if w.count == 0 || s.rttUs > w.maxUs {
			w.maxUs = s.rttUs
		}
		if w.count == 0 || s.rttUs < w.minUs {
			w.minUs = s.rttUs
		}
		w.count++
	}
	if s.rtoUs > 0 {
		w.latestRTOUs = s.rtoUs
	}
	if s.minRTTUs > 0 {
		w.latestMinLifetimeUs = s.minRTTUs
	}
	if s.rttVarUs > 0 {
		w.latestVarUs = s.rttVarUs
	}
}

func (w *RTTWindow) drainAndReset() rttSample {
	if w == nil {
		return rttSample{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.count == 0 {
		if !w.haveLastDrain {
			return rttSample{}
		}
		return rttSample{
			avgMs:         float32(w.lastAvgUs) / 1000,
			maxMs:         float32(w.lastMaxUs) / 1000,
			minMs:         float32(w.lastMinUs) / 1000,
			minLifetimeMs: float32(w.latestMinLifetimeUs) / 1000,
			varMs:         float32(w.latestVarUs) / 1000,
			rtoMs:         float32(w.latestRTOUs) / 1000,
			hasData:       true,
			stale:         true,
		}
	}
	avgUs := uint32(w.sumUs / w.count)
	sample := rttSample{
		avgMs:         float32(avgUs) / 1000,
		maxMs:         float32(w.maxUs) / 1000,
		minMs:         float32(w.minUs) / 1000,
		minLifetimeMs: float32(w.latestMinLifetimeUs) / 1000,
		varMs:         float32(w.latestVarUs) / 1000,
		rtoMs:         float32(w.latestRTOUs) / 1000,
		hasData:       true,
	}
	w.lastAvgUs = avgUs
	w.lastMaxUs = w.maxUs
	w.lastMinUs = w.minUs
	w.haveLastDrain = true
	w.sumUs = 0
	w.count = 0
	w.maxUs = 0
	w.minUs = 0
	return sample
}

// startRTTSampler runs a 100 ms ticker that walks the active session
// snapshot and folds a fresh getsockopt(TCP_INFO) read into each
// session's RTTWindow. ~80 syscalls/sec at 8 sessions, negligible
// CPU cost. Decoupled from the 1 s snapshot emit cadence so the per-
// window max captures sub-second spikes the kernel's smoothed
// tcpi_rtt would otherwise mask.
func (a *App) startRTTSampler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
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
					conn := sessionLoadTCPConn(session)
					if conn == nil {
						continue
					}
					window := sessionLoadRTTWindow(session)
					if window == nil {
						continue
					}
					info, err := readTCPInfo(conn)
					if err != nil {
						continue
					}
					window.fold(info)
				}
			}
		}
	}()
	log.Printf("rtt sampler started (100ms cadence)")
}

// drainSessionRTT pulls the latest 1 s RTT window for a session and
// stamps the six metric fields onto the session map. Called from
// normalizeSessionsForResponse on each broadcast tick.
func drainSessionRTT(session SessionData) {
	window := sessionLoadRTTWindow(session)
	if window == nil {
		return
	}
	sample := window.drainAndReset()
	if !sample.hasData {
		return
	}
	session["client_rtt_ms"] = float64(sample.avgMs)
	session["client_rtt_max_ms"] = float64(sample.maxMs)
	session["client_rtt_min_ms"] = float64(sample.minMs)
	session["client_rtt_min_lifetime_ms"] = float64(sample.minLifetimeMs)
	session["client_rtt_var_ms"] = float64(sample.varMs)
	session["client_rto_ms"] = float64(sample.rtoMs)
	if sample.stale {
		session["client_rtt_stale"] = true
	} else {
		delete(session, "client_rtt_stale")
	}
}
