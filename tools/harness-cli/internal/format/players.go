// Package format owns the human-readable output for the CLI. Each
// command picks a formatter; --json globally swaps in raw JSON output.
//
// Kept separate from the api facade so commands can produce two
// different views of the same record (e.g. `players list` table vs
// the future `players show` per-record detail) without the facade
// caring.
package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// JSON writes any value as pretty JSON. Used by every command's
// --json branch so output is consistent.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// PlayersTable renders the v2 player list as a tab-aligned table.
// First-cut columns; later commands (`players show`) can drill into
// PlayerMetrics for state / buffer / rendition / etc.
func PlayersTable(w io.Writer, players []proxy.PlayerRecord) {
	if len(players) == 0 {
		fmt.Fprintln(w, "no active players")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tDEVICE\tACTIVE PLAY\tREVISION")
	for _, p := range players {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			shortID(p.Id.String()),
			deviceLabel(p),
			activePlayLabel(p),
			shortRev(p.ControlRevision),
		)
	}
	_ = tw.Flush()
}

func shortID(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func shortRev(rev string) string {
	if rev == "" {
		return "—"
	}
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// deviceLabel picks the most operator-meaningful name for a player.
// Tries (in order): labels.device, labels.name, user_agent (truncated).
func deviceLabel(p proxy.PlayerRecord) string {
	if p.Labels != nil {
		if v, ok := (*p.Labels)["device"]; ok && v != "" {
			return v
		}
		if v, ok := (*p.Labels)["name"]; ok && v != "" {
			return v
		}
	}
	if p.UserAgent != nil && *p.UserAgent != "" {
		ua := *p.UserAgent
		if len(ua) > 40 {
			ua = ua[:37] + "..."
		}
		return ua
	}
	return "—"
}

// activePlayLabel returns the short play_id when the player has a
// current play, or "idle" otherwise.
func activePlayLabel(p proxy.PlayerRecord) string {
	if p.CurrentPlay == nil {
		return "idle"
	}
	return shortID(p.CurrentPlay.Id.String())
}
