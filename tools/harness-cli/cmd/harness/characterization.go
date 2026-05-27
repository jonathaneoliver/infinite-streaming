package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
)

// cmdPost is the top-level `harness post …` dispatcher. Currently only
// `characterization` is wired; future writeable surfaces can hang off
// here (e.g. `harness post finding`).
func cmdPost(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New("usage: harness post characterization <path-to-report.json>")
	}
	switch args[0] {
	case "characterization":
		return cmdPostCharacterization(client, args[1:], asJSON)
	}
	return fmt.Errorf("unknown post target: %s", args[0])
}

const characterizationUsage = `harness post characterization <path-to-report.json>

Upload a characterization-test report (the .json file the Go test
framework writes under tests/characterization/modes/artifacts/) to
the forwarder. The server stores it on the characterization_runs
table for the dashboard's Automated Testing page to render.

Required from the embedded report:
  mode, platform, started_at, ended_at, summary, [steps]

Plus an additional wrapper layer:
  --run-id     short run id (e.g. 20260521T160000Z). Falls back to
               parsing the file basename when omitted.
  --test-name  rampup | rampdown | pyramid. Falls back to the
               report's mode field.
  --platform   iphone | ipad-sim | …. Falls back to the report's
               platform field.

Example:
  harness post characterization tests/characterization/modes/artifacts/rampup-iphone-a45a161d-20260521T160000Z.json
`

// reportShape is just enough of the runner.Report fields for the CLI
// to fill in run/test/platform defaults when the user doesn't pass
// them explicitly.
type reportShape struct {
	Mode      string `json:"mode"`
	Platform  string `json:"platform"`
	StartedAt string `json:"started_at"`
}

func cmdPostCharacterization(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(characterizationUsage)
	}
	fs := flag.NewFlagSet("post characterization", flag.ContinueOnError)
	runID := fs.String("run-id", "", "override run id (default: parsed from filename)")
	testName := fs.String("test-name", "", "override test name (default: report.mode)")
	platform := fs.String("platform", "", "override platform (default: report.platform)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("path to report.json is required")
	}
	path := fs.Arg(0)

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var shape reportShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return fmt.Errorf("decode report: %w", err)
	}

	// Fill defaults from the report struct + filename.
	if *runID == "" {
		*runID = runIDFromFilename(path)
	}
	if *runID == "" {
		// Fall back to started_at — UTC timestamp without separators
		// matches the format the test framework already uses.
		if shape.StartedAt != "" {
			*runID = strings.NewReplacer("-", "", ":", "", "T", "T", "Z", "Z").Replace(shape.StartedAt)
		}
	}
	if *testName == "" {
		*testName = shape.Mode
	}
	if *platform == "" {
		*platform = shape.Platform
	}
	if *runID == "" || *testName == "" || *platform == "" {
		return fmt.Errorf("could not derive run_id / test_name / platform; pass --run-id --test-name --platform")
	}

	body, err := json.Marshal(map[string]any{
		"run_id":    *runID,
		"test_name": *testName,
		"platform":  *platform,
		"report":    json.RawMessage(raw),
	})
	if err != nil {
		return err
	}

	url := strings.TrimRight(client.BaseURL, "/") + "/analytics/api/v2/characterization-runs"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if client.BasicAuth != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(client.BasicAuth)))
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if asJSON {
		return format.JSON(os.Stdout, json.RawMessage(respBody))
	}
	var decoded map[string]any
	_ = json.Unmarshal(respBody, &decoded)
	fmt.Printf("uploaded %s/%s/%s → forwarder\n", *platform, *testName, *runID)
	if pid, _ := decoded["player_id"].(string); pid != "" {
		fmt.Printf("  player_id=%s\n", pid)
	}
	if plays, _ := decoded["play_ids"].([]any); len(plays) > 0 {
		ss := make([]string, 0, len(plays))
		for _, p := range plays {
			if s, ok := p.(string); ok {
				ss = append(ss, s)
			}
		}
		fmt.Printf("  play_ids=%s\n", strings.Join(ss, ", "))
	}
	if pass, _ := decoded["passed"].(bool); pass {
		fmt.Printf("  passed=true\n")
	} else {
		fmt.Printf("  passed=false\n")
	}
	return nil
}

// runIDFromFilename extracts the trailing UTC timestamp from filenames
// the test framework writes, e.g.
//
//	rampup-iphone-a45a161d-20260521T160000Z.json
//
// → "20260521T160000Z". Returns empty when no timestamp pattern matches.
func runIDFromFilename(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".json")
	parts := strings.Split(base, "-")
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if len(last) >= 16 && strings.Contains(last, "T") && strings.HasSuffix(last, "Z") {
		return last
	}
	return ""
}
