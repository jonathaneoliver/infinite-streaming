package api

// Target resolution: every mutation/streaming command takes a single
// `<target>` string that operators type by hand. Hard-coding UUIDs is
// horrible; this resolver maps prose-friendly forms onto a canonical
// player_id by scanning the live player list once.
//
// Match precedence (first hit wins, no fallback if a match is found):
//   1. Full UUID — `Id` exact, or `CurrentPlay.Id` exact
//   2. Short hex prefix (>= 6 chars) — player Id or current_play Id
//   3. Labels — value match on `device`, `name`, then any label value
//   4. Player IP / origination IP exact
//   5. User-Agent substring (case-insensitive)
//
// If the target matches zero players we return a structured error so
// commands can render `no match`. If it matches more than one we list
// them in the error so the operator can disambiguate.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// Resolve returns the player_id for the given operator-supplied target.
// Empty target is an error — commands that want "all players" should
// branch on that themselves before calling Resolve.
func (c *Client) Resolve(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("empty target")
	}
	players, err := c.Players(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve %q: list players: %w", target, err)
	}
	return resolveAgainst(target, players)
}

// resolveAgainst is the pure matching core, broken out so it's easy to
// unit-test without standing up an HTTP server.
func resolveAgainst(target string, players []proxy.PlayerRecord) (string, error) {
	lower := strings.ToLower(target)
	short := len(target) >= 6 && isHexPrefix(target)

	// Pass 1: exact-ID matches (UUID or short hex prefix). Highest
	// confidence — stop on first hit.
	for _, p := range players {
		pid := p.Id.String()
		if pid == target {
			return pid, nil
		}
		if p.CurrentPlay != nil && p.CurrentPlay.Id.String() == target {
			return pid, nil
		}
	}
	if short {
		var hits []string
		for _, p := range players {
			pid := p.Id.String()
			compactID := strings.ReplaceAll(pid, "-", "")
			if strings.HasPrefix(compactID, lower) || strings.HasPrefix(pid, lower) {
				hits = append(hits, pid)
				continue
			}
			if p.CurrentPlay != nil {
				cp := strings.ReplaceAll(p.CurrentPlay.Id.String(), "-", "")
				if strings.HasPrefix(cp, lower) {
					hits = append(hits, pid)
				}
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
		if len(hits) > 1 {
			return "", ambiguous(target, hits, players)
		}
	}

	// Pass 2: labels — `device`, `name`, then any value.
	hits := matchLabels(lower, players, "device", "name")
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", ambiguous(target, hits, players)
	}
	hits = matchAnyLabelValue(lower, players)
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", ambiguous(target, hits, players)
	}

	// Pass 3: IPs (player-reported or origination).
	for _, p := range players {
		if p.PlayerIp != nil && *p.PlayerIp == target {
			return p.Id.String(), nil
		}
		if p.OriginationIp != nil && *p.OriginationIp == target {
			return p.Id.String(), nil
		}
	}

	// Pass 4: user-agent substring (case-insensitive). Last resort —
	// substring matches are noisy so we only accept this when the hit
	// set is unique.
	hits = nil
	for _, p := range players {
		if p.UserAgent != nil && strings.Contains(strings.ToLower(*p.UserAgent), lower) {
			hits = append(hits, p.Id.String())
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", ambiguous(target, hits, players)
	}

	return "", fmt.Errorf("no player matches %q (have %d players)", target, len(players))
}

func matchLabels(lower string, players []proxy.PlayerRecord, keys ...string) []string {
	var hits []string
	for _, p := range players {
		if p.Labels == nil {
			continue
		}
		for _, k := range keys {
			if v, ok := (*p.Labels)[k]; ok && strings.ToLower(v) == lower {
				hits = append(hits, p.Id.String())
				break
			}
		}
	}
	return hits
}

func matchAnyLabelValue(lower string, players []proxy.PlayerRecord) []string {
	var hits []string
	for _, p := range players {
		if p.Labels == nil {
			continue
		}
		for _, v := range *p.Labels {
			if strings.ToLower(v) == lower {
				hits = append(hits, p.Id.String())
				break
			}
		}
	}
	return hits
}

func ambiguous(target string, hits []string, players []proxy.PlayerRecord) error {
	byID := make(map[string]proxy.PlayerRecord, len(players))
	for _, p := range players {
		byID[p.Id.String()] = p
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "target %q matched %d players:", target, len(hits))
	for _, id := range hits {
		p := byID[id]
		dev := ""
		if p.Labels != nil {
			if v, ok := (*p.Labels)["device"]; ok {
				dev = " " + v
			}
		}
		fmt.Fprintf(&sb, "\n  %s%s", id, dev)
	}
	sb.WriteString("\n(disambiguate by passing the full UUID)")
	return fmt.Errorf("%s", sb.String())
}

func isHexPrefix(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
