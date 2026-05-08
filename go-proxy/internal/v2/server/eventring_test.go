package server

import (
	"testing"
	"time"
)

func TestEventRing_PublishAndSinceWithinWindow(t *testing.T) {
	r := NewEventRing(100, time.Hour)
	f1 := r.Publish("a", []byte(`"a"`))
	f2 := r.Publish("b", []byte(`"b"`))
	f3 := r.Publish("c", []byte(`"c"`))

	// No Last-Event-ID → no replay.
	if got := r.Since(0); len(got.Frames) != 0 || got.Gap {
		t.Errorf("Since(0) = %+v, want empty", got)
	}

	// Last-Event-ID = top → no missed frames.
	if got := r.Since(f3.ID); len(got.Frames) != 0 || got.Gap {
		t.Errorf("Since(top) = %+v, want empty", got)
	}

	// Last-Event-ID = f1 → replay f2, f3.
	got := r.Since(f1.ID)
	if got.Gap {
		t.Errorf("in-window reconnect reported gap")
	}
	if len(got.Frames) != 2 || got.Frames[0].ID != f2.ID || got.Frames[1].ID != f3.ID {
		t.Errorf("Since(%d) = %+v, want frames f2,f3", f1.ID, got.Frames)
	}
}

func TestEventRing_GapWhenOutOfWindow(t *testing.T) {
	r := NewEventRing(3, time.Hour) // small ring
	r.Publish("a", []byte(`"a"`))   // id=1
	r.Publish("b", []byte(`"b"`))   // id=2
	r.Publish("c", []byte(`"c"`))   // id=3
	r.Publish("d", []byte(`"d"`))   // id=4 (evicts id=1)
	r.Publish("e", []byte(`"e"`))   // id=5 (evicts id=2; ring tail = 3)

	// Client reconnects at id=1, ring tail is 3 → gap.
	got := r.Since(1)
	if !got.Gap {
		t.Errorf("expected gap, got %+v", got)
	}
	if got.MissedFrom != 2 {
		t.Errorf("MissedFrom = %d, want 2", got.MissedFrom)
	}
	if got.MissedTo != 2 {
		// ring tail id=3, so missed = (2,2) — only id=2 was lost.
		t.Errorf("MissedTo = %d, want 2", got.MissedTo)
	}
	if len(got.Frames) != 3 {
		t.Errorf("expected 3 surviving frames, got %d", len(got.Frames))
	}
}

func TestEventRing_SubscribeFanOut(t *testing.T) {
	r := NewEventRing(100, time.Hour)
	_, ch, cancel := r.Subscribe(8)
	defer cancel()

	r.Publish("a", []byte(`"a"`))
	select {
	case f := <-ch:
		if string(f.Payload) != `"a"` {
			t.Errorf("got payload %q, want \"a\"", f.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frame")
	}
}

func TestEventRing_AgeEviction(t *testing.T) {
	r := NewEventRing(100, 10*time.Millisecond)
	r.Publish("old", []byte(`"old"`))
	time.Sleep(30 * time.Millisecond)
	r.Publish("new", []byte(`"new"`))

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.frames) != 1 {
		t.Errorf("after age eviction, len = %d, want 1; frames=%+v", len(r.frames), r.frames)
	}
	if r.frames[0].Type != "new" {
		t.Errorf("survivor type = %q, want 'new'", r.frames[0].Type)
	}
}

func TestParseLastEventID(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
	}{
		{"", 0},
		{"123", 123},
		{"abc", 0},
		{"-1", 0}, // negative isn't uint64
	}
	for _, tt := range tests {
		if got := ParseLastEventID(tt.in); got != tt.want {
			t.Errorf("ParseLastEventID(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
