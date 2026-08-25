package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DeviceCapability is one device-farm device's matchable attributes + live
// availability, distilled from the farm's /device-farm/api/device response
// (issue #948). It is what the scheduler (the sweep dispatcher) reads to decide
// (a) which work is serviceable right now — scenario 3's availability gate — and
// (b) how many free devices of a capability it can pack work across — scenario 2.
type DeviceCapability struct {
	UDID     string `json:"udid"`
	Name     string `json:"name"`
	Platform string `json:"platform"` // "ios" | "tvos" | "android"
	Real     bool   `json:"real"`     // hardware vs simulator
	Version  string `json:"version"`  // OS/SDK version, e.g. "26.5"
	Host     string `json:"host"`

	Busy    bool `json:"busy"`    // in an active session
	Offline bool `json:"offline"` // farm can't reach the node
	Blocked bool `json:"blocked"` // userBlocked — reserved by another consumer
	Booted  bool `json:"booted"`  // sim is running; real devices are always true

	// Free is the single availability verdict: allocatable to a new session
	// right now. A shutdown sim is still Free — the farm boots it on session
	// create — so bootability is captured by Booted, not folded into Free.
	Free bool `json:"free"`
}

// availableDevices fetches the live device-farm roster and returns every device
// as a typed DeviceCapability (issue #948). It does NOT filter — the caller
// picks what it needs (FreeDevices for allocation, the full list to answer "does
// this capability exist at all" for the gate). Returns nil on any transport or
// decode error, so an unreachable farm reads as "no devices" rather than a hard
// failure — the sweep's existing no-device Skip path then applies.
func availableDevices(base string) []DeviceCapability {
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get(base + "/device-farm/api/device")
	if err != nil {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return parseFarmRoster(raw)
}

// parseFarmRoster decodes the farm's /device response into DeviceCapability. Pure
// (no I/O) so the mapping — the availability verdict especially — is unit-testable
// against captured farm JSON. A body that isn't the expected array yields nil.
func parseFarmRoster(raw []byte) []DeviceCapability {
	var all []farmDevice
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	out := make([]DeviceCapability, 0, len(all))
	for _, d := range all {
		// Real devices report no simulator state; treat them as always booted.
		// Sims are booted only when the farm reports state=="Booted".
		booted := d.RealDevice || strings.EqualFold(strings.TrimSpace(d.State), "Booted")
		dc := DeviceCapability{
			UDID:     d.UDID,
			Name:     d.Name,
			Platform: strings.ToLower(strings.TrimSpace(d.Platform)),
			Real:     d.RealDevice,
			Version:  strings.TrimSpace(d.SDK),
			Host:     d.Host,
			Busy:     d.Busy,
			Offline:  d.Offline,
			Blocked:  d.UserBlocked,
			Booted:   booted,
		}
		// Free ⇔ not in a session, reachable, and not reserved by another
		// consumer. Boot state is deliberately NOT a factor — the farm boots a
		// shutdown sim on POST /session.
		dc.Free = !d.Busy && !d.Offline && !d.UserBlocked
		out = append(out, dc)
	}
	return out
}

// cmdDevices prints the live device-farm roster (issue #948): the observable
// face of availableDevices(). Human table by default, `--json` for scripting,
// `--free` to list only the allocatable set. Read-only; no session touched.
func cmdDevices(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	freeOnly := fs.Bool("free", false, "list only devices allocatable right now")
	if err := fs.Parse(args); err != nil {
		return err
	}
	devs := availableDevices(deviceFarmBaseURL())
	if *freeOnly {
		devs = FreeDevices(devs)
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(devs)
	}
	if len(devs) == 0 {
		fmt.Printf("no devices (farm %s unreachable or empty)\n", deviceFarmBaseURL())
		return nil
	}
	fmt.Printf("%-38s  %-8s  %-4s  %-7s  %-6s  %s\n", "UDID", "PLATFORM", "KIND", "VERSION", "STATE", "NAME")
	for _, d := range devs {
		kind := "sim"
		if d.Real {
			kind = "real"
		}
		state := "free"
		switch {
		case d.Busy:
			state = "busy"
		case d.Offline:
			state = "offline"
		case d.Blocked:
			state = "blocked"
		case !d.Booted:
			state = "free*" // free but not yet booted (farm boots on demand)
		}
		fmt.Printf("%-38s  %-8s  %-4s  %-7s  %-6s  %s\n", d.UDID, d.Platform, kind, d.Version, state, d.Name)
	}
	return nil
}

// FreeDevices returns only the devices allocatable to a new session right now.
// This is the roster the concurrent dispatcher (issue #950) sizes its worker
// pool against.
func FreeDevices(devs []DeviceCapability) []DeviceCapability {
	var out []DeviceCapability
	for _, d := range devs {
		if d.Free {
			out = append(out, d)
		}
	}
	return out
}
