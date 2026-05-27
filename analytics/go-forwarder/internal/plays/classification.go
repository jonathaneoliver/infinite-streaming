package plays

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
)

// Classification values accepted by the v2 plays PATCH endpoint and
// the chat tools. `auto` is the request to re-run the auto-classifier
// (the value finally written is `interesting` or `other`).
const (
	ClassificationOther       = "other"
	ClassificationInteresting = "interesting"
	ClassificationFavourite   = "favourite"
)

// SetPlayClassification writes `value` to every row of play_id in
// session_events + network_requests + control_events. When force is
// false, existing `favourite` rows are preserved (the auto-classifier
// must not quietly demote a starred play).
//
// `SETTINGS mutations_sync = 2` makes ALTER UPDATE wait for the
// mutation to apply before returning, so a v2 PATCH response sees the
// settled value (avoids dashboard optimistic-flip-revert flicker).
func SetPlayClassification(ctx context.Context, b Backend, playID, value string, force bool) error {
	if playID == "" {
		return errors.New("play id required")
	}
	whereSafe := "WHERE play_id = {play:String}"
	if !force {
		whereSafe += " AND classification != 'favourite'"
	}
	params := map[string]string{"play": playID, "cls": value}
	const syncSuffix = " SETTINGS mutations_sync = 2"
	updates := []struct {
		label string
		query string
	}{
		{"session_events", fmt.Sprintf("ALTER TABLE %s.%s UPDATE classification = {cls:String} %s"+syncSuffix, b.Database, b.EventsTable, whereSafe)},
		{"network_requests", fmt.Sprintf("ALTER TABLE %s.network_requests UPDATE classification = {cls:String} %s"+syncSuffix, b.Database, whereSafe)},
		{"control_events", fmt.Sprintf("ALTER TABLE %s.control_events UPDATE classification = {cls:String} %s"+syncSuffix, b.Database, whereSafe)},
	}
	for _, u := range updates {
		if _, err := b.queryBytes(ctx, u.query, params); err != nil {
			log.Printf("ALTER UPDATE classification table=%s pid=%s value=%s err=%v",
				u.label, playID, value, err)
			return fmt.Errorf("ALTER UPDATE classification on %s: %w", u.label, err)
		}
	}
	return nil
}

// SetClassification is the session-scoped twin: keyed on (session_id,
// play_id) so the auto-classifier (which drains by session) doesn't
// have to round-trip to a play_id-only key.
//
// Doesn't use mutations_sync = 2 — the auto-classifier doesn't read
// back, so the async default is fine (and cheaper at queue-drain
// rates).
func SetClassification(ctx context.Context, b Backend, sessionID, playID, value string, force bool) error {
	whereSafe := "WHERE session_id = {session:String} AND play_id = {play:String}"
	if !force {
		whereSafe += " AND classification != 'favourite'"
	}
	params := map[string]string{
		"session": sessionID,
		"play":    playID,
		"cls":     value,
	}
	updates := []struct {
		label string
		query string
	}{
		{"session_events", fmt.Sprintf("ALTER TABLE %s.%s UPDATE classification = {cls:String} %s", b.Database, b.EventsTable, whereSafe)},
		{"network_requests", fmt.Sprintf("ALTER TABLE %s.network_requests UPDATE classification = {cls:String} %s", b.Database, whereSafe)},
		{"control_events", fmt.Sprintf("ALTER TABLE %s.control_events UPDATE classification = {cls:String} %s", b.Database, whereSafe)},
	}
	for _, u := range updates {
		if _, err := b.queryBytes(ctx, u.query, params); err != nil {
			log.Printf("ALTER UPDATE classification table=%s sid=%s pid=%s value=%s err=%v",
				u.label, sessionID, playID, value, err)
			return fmt.Errorf("ALTER UPDATE classification on %s: %w", u.label, err)
		}
	}
	return nil
}

// ReclassifyPlay runs the auto-classifier predicate against
// session_events filtered on play_id alone, then writes the result
// via SetPlayClassification. Used by PATCH /api/v2/plays/{id} with
// classification=auto.
func ReclassifyPlay(ctx context.Context, b Backend, playID string, force bool) error {
	if playID == "" {
		return errors.New("play id required")
	}
	probe := fmt.Sprintf(`
		SELECT count() FROM %s.%s
		WHERE play_id = {play:String}
		  AND last_event IN ('user_marked', 'frozen', 'segment_stall', 'restart', 'error')
		FORMAT TSV`, b.Database, b.EventsTable)
	body, err := b.queryBytes(ctx, probe, map[string]string{"play": playID})
	if err != nil {
		return fmt.Errorf("auto-classifier probe: %w", err)
	}
	target := ClassificationOther
	if c := strings.TrimSpace(string(body)); c != "" && c != "0" {
		target = ClassificationInteresting
	}
	return SetPlayClassification(ctx, b, playID, target, force)
}

// ReclassifySession is the session-scoped twin of ReclassifyPlay,
// used by the auto-classifier queue drain. play_id may be empty for
// pre-stamp legacy rows.
func ReclassifySession(ctx context.Context, b Backend, sessionID, playID string, force bool) error {
	if sessionID == "" {
		return errors.New("session id required")
	}
	probe := fmt.Sprintf(`
		SELECT count() FROM %s.%s
		WHERE session_id = {session:String} AND play_id = {play:String}
		  AND last_event IN ('user_marked', 'frozen', 'segment_stall', 'restart', 'error')
		FORMAT TSV`, b.Database, b.EventsTable)
	body, err := b.queryBytes(ctx, probe, map[string]string{
		"session": sessionID,
		"play":    playID,
	})
	if err != nil {
		return fmt.Errorf("auto-classifier probe: %w", err)
	}
	count := strings.TrimSpace(string(body))
	target := ClassificationOther
	if count != "" && count != "0" {
		target = ClassificationInteresting
	}
	return SetClassification(ctx, b, sessionID, playID, target, force)
}
