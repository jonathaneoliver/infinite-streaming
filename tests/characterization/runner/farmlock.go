package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Farm mutex for the real iPhone on the HYBRID path.
//
// A real iOS device is driven OFF the device farm (a plain Appium on :4799, see
// directIOSAppiumURL) — the farm can't bring up WDA on iOS 17+/26. That means
// the farm never marks it busy, so nothing stops two parallel runs from grabbing
// the same physical phone at once. We borrow the farm's manual block flag
// (`userBlocked`, toggled by POST /device-farm/api/{block,unblock}) purely as a
// cross-process lock: acquire = wait-until-free then /block; release = /unblock.
//
// NOTE the harness CLI reap only unblocks `busy` devices, not `userBlocked`
// ones, so release is owned HERE (Close/discardSession), not by the reap.

// deviceFarmBaseURL is the appium-device-farm server base (the plugin host) —
// CHAR_APPIUM_URL or the :4723 default, mirroring the harness CLI. Distinct from
// a real device's direct-Appium URL (:4799 on the hybrid path).
func deviceFarmBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("CHAR_APPIUM_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:4723"
}

// farmLockWait is the max time acquireFarmLock waits for the phone to free up.
// Parallel runs serialize on ONE device, so a later run may wait out the whole
// earlier run — keep it generous. CHAR_IOS_FARM_WAIT_SEC overrides; default 20m.
func farmLockWait() time.Duration {
	if v := strings.TrimSpace(os.Getenv("CHAR_IOS_FARM_WAIT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 20 * time.Minute
}

type farmDeviceStatus struct {
	UDID        string `json:"udid"`
	Host        string `json:"host"`
	Busy        bool   `json:"busy"`
	UserBlocked bool   `json:"userBlocked"`
}

func farmRoster(ctx context.Context, client *http.Client, base string) ([]farmDeviceStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/device-farm/api/device", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var all []farmDeviceStatus
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	return all, nil
}

func farmDeviceByUDID(roster []farmDeviceStatus, udid string) (farmDeviceStatus, bool) {
	for _, d := range roster {
		if strings.EqualFold(d.UDID, udid) {
			return d, true
		}
	}
	return farmDeviceStatus{}, false
}

func farmBlockCall(ctx context.Context, client *http.Client, base, action, udid, host string) error {
	body, _ := json.Marshal(map[string]string{"udid": udid, "host": host})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/device-farm/api/"+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("farm %s %s: HTTP %d", action, udid, resp.StatusCode)
	}
	return nil
}

// acquireFarmLock waits until the farm shows udid free (not busy, not
// userBlocked), then /block-s it as a cross-run mutex and returns the host to
// /unblock with later. Returns ("", nil) — no lock, caller drives anyway — when
// the farm is unreachable or doesn't list this device (best-effort; the lock is
// an optimization, never a hard gate that could wedge a lone run). Returns an
// error only when the device stays blocked past the wait budget, so a genuinely
// contended phone surfaces instead of silently double-driving.
func acquireFarmLock(ctx context.Context, udid string) (host string, err error) {
	udid = strings.TrimSpace(udid)
	if udid == "" {
		return "", nil
	}
	base := deviceFarmBaseURL()
	client := &http.Client{Timeout: 6 * time.Second}
	deadline := time.Now().Add(farmLockWait())
	for {
		roster, rerr := farmRoster(ctx, client, base)
		if rerr != nil {
			// Farm not reachable — skip locking, drive anyway (single-run case).
			return "", nil
		}
		dev, ok := farmDeviceByUDID(roster, udid)
		if !ok {
			// Device not in the farm roster — nothing to lock against.
			return "", nil
		}
		if !dev.Busy && !dev.UserBlocked {
			if berr := farmBlockCall(ctx, client, base, "block", dev.UDID, dev.Host); berr != nil {
				return "", nil // best-effort: couldn't block, drive anyway
			}
			return dev.Host, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("device %s still blocked in the farm after %s (another run is using it, or a leaked lock — POST /device-farm/api/unblock to clear)", udid, farmLockWait())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// releaseFarmLock clears the userBlocked flag this run set in acquireFarmLock.
// Best-effort with its own short context so it runs even when the caller's ctx
// is already done (the common teardown case). No-op when host is empty (no lock
// was taken).
func releaseFarmLock(udid, host string) {
	if strings.TrimSpace(udid) == "" || strings.TrimSpace(host) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 6 * time.Second}
	_ = farmBlockCall(ctx, client, deviceFarmBaseURL(), "unblock", udid, host)
}
