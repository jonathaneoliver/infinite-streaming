package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

// Real-device preflight helpers. iOS 17+ hardware needs an established go-ios
// RemoteXPC tunnel for ANY app launch/instrumentation; without it every launch
// fails with a cryptic deep-in-appium error. These let the fleet resolver fail
// LOUDLY and early with an actionable message instead.

// tunnelListEntry is the shape of one `ios tunnel ls` JSON record we care about.
type tunnelListEntry struct {
	UDID string `json:"udid"`
}

// tunnelListHasUDID reports whether the `ios tunnel ls` JSON output contains an
// established tunnel for udid. Pure (no I/O) so the parse is unit-testable; an
// empty list ("[]") or a udid not present → false.
func tunnelListHasUDID(raw []byte, udid string) bool {
	var entries []tunnelListEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false
	}
	for _, e := range entries {
		if strings.EqualFold(strings.TrimSpace(e.UDID), strings.TrimSpace(udid)) {
			return true
		}
	}
	return false
}

// IOSTunnelUp reports whether go-ios has an active RemoteXPC tunnel for udid
// (i.e. `ios tunnel ls` lists it). Returns an error (not a false negative) when
// the `ios` tool is unavailable or the command fails, so a caller can distinguish
// "tunnel is genuinely down" (up=false, err=nil) from "couldn't check" (err!=nil)
// and avoid a false-positive guard failure.
func IOSTunnelUp(ctx context.Context, udid string) (up bool, err error) {
	if _, lookErr := exec.LookPath("ios"); lookErr != nil {
		return false, fmt.Errorf("go-ios `ios` not on $PATH: %w", lookErr)
	}
	out, runErr := exec.CommandContext(ctx, "ios", "tunnel", "ls").Output()
	if runErr != nil {
		return false, fmt.Errorf("ios tunnel ls: %w", runErr)
	}
	return tunnelListHasUDID(out, udid), nil
}

// RealDeviceAppiumURL resolves the plain-Appium base URL a REAL iOS device is
// driven through — reusing directIOSAppiumURL (CHAR_IOS_DIRECT_APPIUM_URL wins,
// else :4799 under the farm) so the preflight checks the SAME endpoint the
// launcher will use. Returns "" when the farm is off (the operator points
// APPIUM_URL at their own plain Appium) — the caller then skips the check.
func RealDeviceAppiumURL() string { return directIOSAppiumURL("") }

// AppiumReachable reports whether an Appium server answers GET /status at base.
// Used to preflight the real-device :4799 server before a run so its absence
// fails loudly up front instead of as a create-session error mid bring-up.
func AppiumReachable(ctx context.Context, base string) bool {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/status", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
