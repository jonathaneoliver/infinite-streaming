package plays

import (
	"context"
	"strings"
	"testing"
)

// TestGetControlEventsRequiresDiscriminator locks the safety invariant: a
// control_events query with no discriminating predicate (player_id / play_id /
// event / labels_has) is refused before it can scan the whole table. The
// rejection returns before any ClickHouse call, so a zero-value Backend is
// fine — we only exercise the guard. #671.
func TestGetControlEventsRequiresDiscriminator(t *testing.T) {
	_, err := GetControlEvents(context.Background(), Backend{}, ControlEventsFilter{})
	if err == nil {
		t.Fatal("empty filter must be rejected (would scan the whole control_events table)")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected a 'required' guard error, got: %v", err)
	}
}
