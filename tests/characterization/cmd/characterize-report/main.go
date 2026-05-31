// Command characterize-report aggregates per-test characterization JSON
// reports into a single Markdown matrix.
//
//	go run ./cmd/characterize-report <artifacts-dir>
//
// Walks the directory for *.json files matching the Report schema. Groups
// by (mode, platform). Emits one Markdown table per mode covering every
// platform that ran it. Useful for a CI run that fans out across 4
// platforms and 7 modes — you get one report to scan instead of 28 files.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

func main() {
	out := flag.String("out", "-", "output path for combined Markdown (default stdout)")
	charts := flag.Bool("charts", false, "also write a Chart.js HTML next to every report JSON")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: characterize-report [-out FILE] [-charts] <artifacts-dir>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	root := flag.Arg(0)

	reports, err := loadReports(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(reports) == 0 {
		fmt.Fprintf(os.Stderr, "no report files found under %s\n", root)
		os.Exit(1)
	}

	if *charts {
		for _, r := range reports {
			base := fmt.Sprintf("%s-%s-%s", r.Mode, r.Platform, r.StartedAt.UTC().Format("20060102T150405Z"))
			path, err := runner.WriteChart(root, base, &r)
			if err != nil {
				fmt.Fprintf(os.Stderr, "chart %s: %v\n", base, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "chart: %s\n", path)
		}
	}

	md := renderMatrix(reports)
	if *out == "-" || *out == "" {
		fmt.Print(md)
		return
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
}

func loadReports(root string) ([]runner.Report, error) {
	var reports []runner.Report
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		var r runner.Report
		if err := json.Unmarshal(raw, &r); err != nil {
			// Skip files that aren't characterization reports — the
			// artifacts dir may also hold logs / .md / unrelated JSON.
			return nil
		}
		if r.Mode == "" {
			return nil
		}
		reports = append(reports, r)
		return nil
	})
	return reports, err
}

func renderMatrix(reports []runner.Report) string {
	// Index: mode → platform → report (most recent wins on tie).
	type key struct {
		mode     string
		platform runner.Platform
	}
	idx := make(map[key]runner.Report)
	for _, r := range reports {
		k := key{r.Mode, r.Platform}
		if prev, ok := idx[k]; !ok || r.StartedAt.After(prev.StartedAt) {
			idx[k] = r
		}
	}

	modes := uniqueModes(reports)
	platforms := uniquePlatforms(reports)

	var b strings.Builder
	fmt.Fprintf(&b, "# Characterization matrix\n\n")
	fmt.Fprintf(&b, "%d reports, %d modes, %d platforms.\n\n", len(reports), len(modes), len(platforms))

	for _, mode := range modes {
		fmt.Fprintf(&b, "## %s\n\n", mode)
		fmt.Fprintf(&b, "| platform | stalls | stall_s | shifts | dropped | buf min/max | bitrate min/mean/max |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
		for _, plat := range platforms {
			r, ok := idx[key{mode, plat}]
			if !ok {
				fmt.Fprintf(&b, "| %s | — | — | — | — | — | — |\n", plat)
				continue
			}
			s := r.Summary
			fmt.Fprintf(&b, "| %s | %d | %.1f | %d | %d | %.1f / %.1f | %.2f / %.2f / %.2f |\n",
				plat,
				s.TotalStalls, s.TotalStallSeconds, s.ProfileShifts, s.FramesDropped,
				s.MinBufferDepthS, s.MaxBufferDepthS,
				s.MinBitrateMbps, s.MeanBitrateMbps, s.MaxBitrateMbps,
			)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func uniqueModes(rs []runner.Report) []string {
	seen := map[string]bool{}
	for _, r := range rs {
		seen[r.Mode] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func uniquePlatforms(rs []runner.Report) []runner.Platform {
	seen := map[runner.Platform]bool{}
	for _, r := range rs {
		seen[r.Platform] = true
	}
	out := make([]runner.Platform, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
