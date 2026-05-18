package eventclass

// Rate-shift classifier — detects bitrate up/downshifts when the
// player reports a rate_shift_down / rate_shift_up last_event AND the
// previous and current video_bitrate_mbps are both positive (matches
// the legacy SQL guards: `prev_bitrate > 0 AND video_bitrate_mbps > 0`).

func init() {
	RegisterSnapshot("snapshot_rate", snapshotRateClassifier{})
}

type snapshotRateClassifier struct{}

func (snapshotRateClassifier) Classify(prev, cur *Snapshot) []Event {
	if cur == nil {
		return nil
	}
	// Issue #470: one POST per transition. Direction is in the event
	// name (rate_shift_up / rate_shift_down) — clients dropped the
	// parallel video_bitrate_change POST that previously duplicated
	// the same observation under a different name.
	if cur.LastEvent != "rate_shift_up" && cur.LastEvent != "rate_shift_down" {
		return nil
	}
	// Prefer the player-supplied from/to fields — clients track the
	// actual transition internally and ship both halves in extras.
	from, to := cur.RateFromMbps, cur.RateToMbps
	if from <= 0 || to <= 0 {
		// Fallback for clients (or replayed historical rows) that
		// don't carry from/to extras.
		if prev == nil || prev.VideoBitrate <= 0 || cur.VideoBitrate <= 0 {
			return nil
		}
		from, to = prev.VideoBitrate, cur.VideoBitrate
	}
	base := Event{
		Ts: cur.Ts, PlayerID: cur.PlayerID, PlayID: cur.PlayID,
		AttemptID: cur.AttemptID, SessionID: cur.SessionID,
		Classification: cur.Classification,
		Info:           FormatBitrateShift(from, to),
	}
	if cur.LastEvent == "rate_shift_down" {
		base.Type = TypeDownshift
	} else {
		base.Type = TypeUpshift
	}
	return []Event{base}
}
