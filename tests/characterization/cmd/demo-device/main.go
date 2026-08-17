// demo-device drives the player app for the narrated demo recorder.
//
// The demo opens on an empty session panel and a phone on its home screen, then
// playback starts while the camera is rolling. That needs the app brought to
// home at one moment and playback started at ANOTHER, chosen by the recorder —
// so this is one long-lived process with two phases rather than two commands.
//
// The Appium session lives in the launcher's memory. Two separate process
// invocations would create two sessions and the second would not know about the
// app state the first left behind, so the phases are sequenced over stdin:
//
//	$ demo-device -clip fpv5_p200_h264_6s
//	READY <udid> <label>          app relaunched, sitting on the home picker
//	> play                        (recorder writes this when it wants playback)
//	PLAYING
//	> quit
//	BYE
//
// Every line this prints is a protocol line for the caller to parse; progress
// chatter goes to stderr so stdout stays machine-readable.
//
// Nothing here is demo-specific beyond the sequencing — it is a thin shell over
// runner.AppiumLauncher, which the characterization suite already uses to drive
// this same phone.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

func main() {
	var (
		clip     = flag.String("clip", "", "content name to play, e.g. fpv5_p200_h264_6s; empty uses continue-watching")
		udid     = flag.String("udid", "", "device UDID; defaults to $CHARACTERIZATION_DEVICE_UDID, then $IPHONE_XCODE_ID")
		platform = flag.String("platform", "iphone", "runner platform: iphone | ipad | ipad-sim | androidtv")
		timeout  = flag.Duration("timeout", 6*time.Minute, "ceiling for launch-to-home")
		list     = flag.Bool("list", false, "list discoverable devices and exit")
		tiles    = flag.Bool("list-tiles", false, "launch to home, print the accessibility ids on screen, exit")
	)
	flag.Parse()

	// The HARDWARE UDID, not the CoreDevice identifier. Using the wrong one
	// makes the device silently fail to match and the run picks something else
	// (or nothing) — the same trap the characterization README calls out.
	if *udid == "" {
		*udid = firstNonEmpty(os.Getenv("CHARACTERIZATION_DEVICE_UDID"), os.Getenv("IPHONE_XCODE_ID"))
	}

	a := runner.NewAppiumLauncher()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	devs, err := a.Discover(ctx)
	if err != nil {
		fatal("discover: %v", err)
	}
	if *list {
		for _, d := range devs {
			fmt.Printf("%s\t%s\t%s\n", d.Platform, d.UDID, d.Label)
		}
		return
	}

	dev, ok := pick(devs, runner.Platform(*platform), *udid)
	if !ok {
		fmt.Fprintf(os.Stderr, "no %s device matching %q. Discovered:\n", *platform, *udid)
		for _, d := range devs {
			fmt.Fprintf(os.Stderr, "  %s\t%s\t%s\n", d.Platform, d.UDID, d.Label)
		}
		os.Exit(1)
	}

	// Resolve BEFORE launching. A bad clip name should cost a second, not a
	// three-minute WDA build followed by a silent fallback to the wrong video.
	clipID, err := clipIDFromContent(ctx, harnessBase(), *clip)
	if err != nil {
		fatal("resolve clip: %v", err)
	}
	if *clip != "" {
		fmt.Fprintf(os.Stderr, "clip %s → home-tile-%s\n", *clip, clipID)
	}

	fmt.Fprintf(os.Stderr, "launching %s to home…\n", dev)
	sess, err := a.LaunchToHome(ctx, dev)
	if err != nil {
		fatal("launch to home: %v", err)
	}
	// Close tears down the Appium session and releases the farm lock on the
	// physical phone. Skipping it wedges the NEXT run with a stale session that
	// looks exactly like a device fault.
	defer a.Close()

	if *tiles {
		src, err := a.PageSource(ctx, dev)
		if err != nil {
			fatal("page source: %v", err)
		}
		ids := accessibilityIDs(src)
		fmt.Fprintf(os.Stderr, "%d identifiers on screen:\n", len(ids))
		for _, id := range ids {
			fmt.Println(id)
		}
		return
	}

	fmt.Printf("READY %s %s\n", dev.UDID, dev.Label)
	os.Stdout.Sync()

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch strings.TrimSpace(in.Text()) {
		case "play":
			// Fresh context: the launch ceiling may already be spent, and the
			// recorder decides when this happens — possibly minutes later.
			pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err := a.ResumePlaybackClip(pctx, dev, clipID)
			pcancel()
			if err != nil {
				fmt.Printf("ERR %v\n", err)
				os.Stdout.Sync()
				continue
			}
			fmt.Println("PLAYING")
			os.Stdout.Sync()

		case "quit", "":
			fmt.Println("BYE")
			return

		default:
			fmt.Println("ERR unknown command")
			os.Stdout.Sync()
		}
	}
	_ = sess
}

// clipIDFromContent resolves a CONTENT NAME to the app's clipId — the string
// the home tiles are actually identified by — by ASKING THE SERVER, which is
// the same place the app gets it (Models.swift decodes `clip_id` and only
// derives one when the server omits it).
//
// Deriving it locally is where this went wrong twice:
//
//   - Passing the raw name builds home-tile-fpv5_p200_h264_6s, which matches
//     nothing. ResumePlaybackClip then times out after 30s and falls back to
//     the continue-watching hero, streaming whatever that resolves to while
//     reporting success — how a take asking for fpv5_p200_h264_6s recorded
//     bucks_bunny_p200_h264.
//   - modes.clipIDFromContent truncates at "_p200_", yielding "fpv5" for
//     fpv5_p200_h264_6s. The server says "fpv5_6s". The segment suffix lives
//     AFTER the codec, so truncating drops it and the tile misses again.
//
// A miss is never loud — it is always the wrong video, played confidently. So
// this asks rather than guesses, and says so when it cannot.
func clipIDFromContent(ctx context.Context, base, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/content", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var items []struct {
		Name   string `json:"name"`
		ClipID string `json:"clip_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return "", fmt.Errorf("decode /api/content: %w", err)
	}
	for _, it := range items {
		if strings.EqualFold(it.Name, name) && it.ClipID != "" {
			return it.ClipID, nil
		}
	}
	return "", fmt.Errorf("%q not in the catalogue (%d items)", name, len(items))
}

// accessibilityIDs pulls the `name=` attributes out of an XCUITest page-source
// dump, deduped and sorted. XCUITest surfaces a view's accessibilityIdentifier
// as `name` when no label overrides it, which is what "accessibility id"
// locators match — so these are exactly the strings -clip can target.
func accessibilityIDs(xml string) []string {
	re := regexp.MustCompile(`name="([^"]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(xml, -1) {
		if id := m[1]; !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func harnessBase() string {
	if v := strings.TrimSpace(os.Getenv("HARNESS_BASE_URL")); v != "" {
		return v
	}
	return "https://dev.jeoliver.com:21000"
}

func pick(devs []runner.Device, p runner.Platform, udid string) (runner.Device, bool) {
	for _, d := range devs {
		if d.Platform != p {
			continue
		}
		if udid == "" || d.UDID == udid {
			return d, true
		}
	}
	return runner.Device{}, false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
