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
	if cur == nil || prev == nil {
		// First row of a play has no prev — can't compute a shift
		// without a baseline, so emit nothing (legacy SQL's
		// lagInFrame would have left prev_bitrate = video_bitrate
		// and the > 0 guard would still trip but the info string
		// would collapse to "X.XX→X.XX" — we just drop the row).
		return nil
	}
	if prev.VideoBitrate <= 0 || cur.VideoBitrate <= 0 {
		return nil
	}
	base := Event{
		Ts: cur.Ts, PlayerID: cur.PlayerID, PlayID: cur.PlayID,
		AttemptID: cur.AttemptID, SessionID: cur.SessionID,
		Classification: cur.Classification,
		Info:           FormatBitrateShift(prev.VideoBitrate, cur.VideoBitrate),
	}
	switch cur.LastEvent {
	case "rate_shift_down":
		base.Type = TypeDownshift
		return []Event{base}
	case "rate_shift_up":
		base.Type = TypeUpshift
		return []Event{base}
	}
	return nil
}
