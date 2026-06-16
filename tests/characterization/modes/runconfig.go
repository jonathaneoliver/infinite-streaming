package modes

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// runConfig captures the per-run TEST CONFIGURATION axes (not player
// behaviour): which segment length to cold-start on, whether the on-device
// LocalHTTPProxy hop is enabled, and the server-side active transfer timeout.
// Each axis is forced deterministically (a launch arg or a proxy PATCH) and
// stamped onto the play as a label so archived runs are self-describing and
// queryable (e.g. `harness query plays --label-has info=xfer_timeout_off`).
//
// Env axes (all optional):
//
//	CHAR_SEGMENT          2s|6s|ll      default: app's current (Appium only)
//	CHAR_LOCAL_PROXY      on|off        default: on        (Appium only)
//	CHAR_TRANSFER_TIMEOUT <seconds>     default: 20        (0 = off; pyramid forces 6)
type runConfig struct {
	segment     string        // label form: "" | 2s | 6s | ll
	localProxy  bool          // default true
	xferTimeout time.Duration // active transfer timeout on segments; 0 = off
	appium      bool
}

var segmentRawValue = map[string]string{"2s": "s2", "6s": "s6", "ll": "ll"}

// readRunConfig parses the CHAR_* axes. isAppium gates the launch-arg axes
// (segment, LocalProxy) which need XCUITest processArguments — they're
// logged-and-ignored on non-Appium launchers.
func readRunConfig(t *testing.T, isAppium bool) runConfig {
	t.Helper()
	cfg := runConfig{localProxy: true, xferTimeout: 20 * time.Second, appium: isAppium}

	if v := strings.TrimSpace(os.Getenv("CHAR_SEGMENT")); v != "" {
		if _, ok := segmentRawValue[v]; !ok {
			t.Fatalf("CHAR_SEGMENT must be one of 2s|6s|ll (got %q)", v)
		}
		if isAppium {
			cfg.segment = v
		} else {
			t.Logf("CHAR_SEGMENT=%s ignored — requires the Appium launcher", v)
		}
	}

	if v := strings.TrimSpace(os.Getenv("CHAR_LOCAL_PROXY")); v != "" {
		on, ok := parseOnOff(v)
		if !ok {
			t.Fatalf("CHAR_LOCAL_PROXY must be on|off (got %q)", v)
		}
		if isAppium {
			cfg.localProxy = on
		} else if !on {
			t.Logf("CHAR_LOCAL_PROXY=%s ignored — requires the Appium launcher", v)
		}
	}

	if v := strings.TrimSpace(os.Getenv("CHAR_TRANSFER_TIMEOUT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			t.Fatalf("CHAR_TRANSFER_TIMEOUT must be a non-negative integer (seconds), got %q", v)
		}
		cfg.xferTimeout = time.Duration(n) * time.Second
	}
	return cfg
}

// launchArgs returns the XCUITest processArguments that force this config at
// cold launch: the segment (-is.segment) and, when LocalProxy is disabled,
// -is.flag.local_proxy 0. iOS folds these into UserDefaults (NSArgumentDomain,
// highest precedence) so loadFlags reads them at init. Empty ⇒ nothing forced.
func (c runConfig) launchArgs() []string {
	if !c.appium {
		return nil
	}
	var args []string
	if c.segment != "" {
		args = append(args, "-is.segment", segmentRawValue[c.segment])
	}
	if !c.localProxy {
		// LocalHTTPProxy is the on-device hop (rewrites origin → 127.0.0.1).
		// Off ⇒ AVPlayer connects straight to the origin (still a go-proxy
		// port, so the player stays visible + shapeable); tests whether that
		// hop is implicated in mid-stream wedges.
		args = append(args, "-is.flag.local_proxy", "0")
	}
	return args
}

// labels returns the config labels to stamp on the play. Values are single
// tokens — the forwarder label encoding forbids ',' and '='.
func (c runConfig) labels() map[string]string {
	m := map[string]string{"localproxy": boolOnOff(c.localProxy)}
	if c.segment != "" {
		m["segment"] = c.segment
	}
	if c.xferTimeout > 0 {
		m["xfer_timeout"] = fmt.Sprintf("%ds", int(c.xferTimeout.Seconds()))
	} else {
		m["xfer_timeout"] = "off"
	}
	return m
}

// applyServerSide arms (or clears, when xferTimeout == 0) the proxy-side
// active transfer timeout on this player's segment fetches. Call after the
// player is bound + heartbeating.
func (c runConfig) applyServerSide(ctx context.Context, sess *runner.Session) error {
	return sess.SetSegmentTimeout(ctx, c.xferTimeout)
}

func parseOnOff(v string) (on, ok bool) {
	switch strings.ToLower(v) {
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	}
	return false, false
}

func boolOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
