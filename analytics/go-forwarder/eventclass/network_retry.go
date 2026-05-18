package eventclass

import (
	"sync"
	"time"
)

// Request-retry classifier — fires when the same URL is fetched again
// within 1–4000 ms of the previous fetch. Ports the legacy SQL's
// request_retry UNION branch which used a window function over
// PARTITION BY url.
//
// State: per-URL last-fetch timestamp. Retention is bounded by the
// `retryWindow` GC sweep below — entries older than 5 minutes are
// reaped so the map doesn't grow unbounded for long-running plays
// that fetch many distinct URLs.

func init() {
	rc := &networkRetryClassifier{
		lastFetch: make(map[string]time.Time),
	}
	RegisterNetwork("network_retry", rc)
}

const (
	retryMinMs      = 1
	retryMaxMs      = 4000
	retryGCInterval = 5 * time.Minute
	retryGCAge      = 5 * time.Minute
)

type networkRetryClassifier struct {
	mu          sync.Mutex
	lastFetch   map[string]time.Time
	lastGCAt    time.Time
}

func (n *networkRetryClassifier) ClassifyRequest(req *NetworkRequest) []Event {
	if req == nil || req.URL == "" {
		return nil
	}
	curTime := parseChTs(req.Ts)
	if curTime.IsZero() {
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	// Periodic GC to keep the map bounded. Cheap: amortised over
	// every incoming network row.
	if curTime.Sub(n.lastGCAt) > retryGCInterval {
		cutoff := curTime.Add(-retryGCAge)
		for k, v := range n.lastFetch {
			if v.Before(cutoff) {
				delete(n.lastFetch, k)
			}
		}
		n.lastGCAt = curTime
	}

	var emit []Event
	if prevTime, ok := n.lastFetch[req.URL]; ok {
		delta := curTime.Sub(prevTime).Milliseconds()
		if delta >= retryMinMs && delta <= retryMaxMs {
			emit = append(emit, Event{
				Ts: req.Ts, PlayerID: req.PlayerID, PlayID: req.PlayID,
				AttemptID: req.AttemptID, SessionID: req.SessionID,
				Classification: req.Classification,
				Type:           TypeRequestRetry,
				Info:           req.Method + " " + req.Path,
			})
		}
	}
	n.lastFetch[req.URL] = curTime
	return emit
}
