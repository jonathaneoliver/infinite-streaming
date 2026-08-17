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
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

func main() {
	var (
		clip     = flag.String("clip", "", "content id to play (tapped as home-tile-<clip>); empty uses continue-watching")
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
			err := a.ResumePlaybackClip(pctx, dev, *clip)
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
