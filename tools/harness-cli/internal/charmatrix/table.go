package charmatrix

import (
	"fmt"
	"strings"
)

// ArmResult is one arm's measured outcome, filled by the runner after the probe
// plays and the events archive is read back. It is the row RenderTable prints.
type ArmResult struct {
	Arm         *Arm
	PlayerID    string
	PlayID      string
	IntendedOff float64 // intended live_offset (0 ⇒ not a live-offset arm)
	AchievedOff float64 // achieved offset read from the events archive
	HasOffset   bool    // an offset sample existed
	Landed      bool    // achieved ~ intended within tolerance (manipulation check)
	Verdict     string  // optional QoE verdict/label summary
	Note        string  // one-line human/oracle note
	Err         string  // non-empty ⇒ the arm could not be measured (bootstrap/probe/query failed)
}

// RenderTable formats every arm result as a fixed-width table. Per the project's
// full-tables rule it reproduces every column; the lever/offset columns read as
// "-" when the arm has no live_offset. The header line names the spec.
func RenderTable(specName string, results []ArmResult) string {
	rows := make([][]string, 0, len(results)+1)
	header := []string{"#", "id", "platform", "seg", "proto", "lever", "intended", "achieved", "landed", "verdict", "play_id", "note"}
	rows = append(rows, header)

	for i, r := range results {
		a := r.Arm
		lever, intended := "-", "-"
		if off, ok := a.IntendedLiveOffset(); ok {
			lever = a.Lever
			if lever == "" {
				lever = leverProxy
			}
			intended = formatNum(off)
		}
		achieved := "-"
		if r.HasOffset {
			achieved = formatNum(r.AchievedOff)
		}
		// landed is only meaningful for a live-offset arm that actually produced
		// an offset sample — otherwise (dry run, query gap) leave it "-" rather
		// than reading a no-data default as a pass/fail.
		landed := "-"
		if _, ok := a.IntendedLiveOffset(); ok && r.HasOffset {
			landed = boolMark(r.Landed)
		}
		note := r.Note
		if r.Err != "" {
			note = "ERR: " + r.Err
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", i+1),
			a.ID,
			dash(a.Platform),
			dash(a.Segment),
			dash(a.Protocol),
			lever,
			intended,
			achieved,
			landed,
			dash(r.Verdict),
			shortID(r.PlayID),
			note,
		})
	}

	widths := colWidths(rows)
	var b strings.Builder
	fmt.Fprintf(&b, "matrix: %s (%d arm%s)\n", specName, len(results), plural(len(results)))
	for ri, row := range rows {
		for ci, cell := range row {
			if ci > 0 {
				b.WriteString("  ")
			}
			b.WriteString(padRight(cell, widths[ci]))
		}
		b.WriteString("\n")
		if ri == 0 {
			// underline the header
			for ci := range row {
				if ci > 0 {
					b.WriteString("  ")
				}
				b.WriteString(strings.Repeat("-", widths[ci]))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func colWidths(rows [][]string) []int {
	if len(rows) == 0 {
		return nil
	}
	w := make([]int, len(rows[0]))
	for _, row := range rows {
		for ci, cell := range row {
			if len(cell) > w[ci] {
				w[ci] = len(cell)
			}
		}
	}
	return w
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func boolMark(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return dash(id)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
