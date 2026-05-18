package eventclass

import "strconv"

// Counter-bump classifier — every monotonically-increasing failure
// counter on the snapshot row that ticks up between (prev, cur) emits
// one event tagged with the corresponding type. Ports the legacy
// SQL's `base WHERE rn > 1 AND <counter> > prev_<counter>` UNION
// branches.
//
// Also covers:
//   - transport_fault_active 0→1 / 1→0 edge (fault_on / fault_off)
//   - player_error changes to a new non-empty value (error)

func init() {
	RegisterSnapshot("snapshot_failures", snapshotFailuresClassifier{})
}

type snapshotFailuresClassifier struct{}

func (snapshotFailuresClassifier) Classify(prev, cur *Snapshot) []Event {
	if cur == nil || prev == nil {
		// Need a predecessor for every check in this file.
		return nil
	}
	base := Event{
		Ts: cur.Ts, PlayerID: cur.PlayerID, PlayID: cur.PlayID,
		AttemptID: cur.AttemptID, SessionID: cur.SessionID,
		Classification: cur.Classification,
	}
	var out []Event

	// Counter-bump table — type ⇄ counter accessor.
	bumps := []struct {
		typ      string
		cur, old uint32
		info     string
	}{
		{TypeMasterManifestFailure, cur.MasterManifestConsecutiveFailures, prev.MasterManifestConsecutiveFailures, ""},
		{TypeAllFailure, cur.AllConsecutiveFailures, prev.AllConsecutiveFailures, ""},
		{TypeManifestFailure, cur.ManifestConsecutiveFailures, prev.ManifestConsecutiveFailures, ""},
		{TypeSegmentFailure, cur.SegmentConsecutiveFailures, prev.SegmentConsecutiveFailures, ""},
		{TypeTransportFailure, cur.TransportConsecutiveFailures, prev.TransportConsecutiveFailures, ""},
		{TypeTransferActiveTimeout, cur.FaultCountTransferActiveTimeout, prev.FaultCountTransferActiveTimeout, ""},
		{TypeTransferIdleTimeout, cur.FaultCountTransferIdleTimeout, prev.FaultCountTransferIdleTimeout, ""},
		{TypeLoopServer, cur.LoopCountServer, prev.LoopCountServer, "loop " + strconv.FormatUint(uint64(cur.LoopCountServer), 10)},
	}
	for _, b := range bumps {
		if b.cur > b.old {
			e := base
			e.Type = b.typ
			e.Info = b.info
			out = append(out, e)
		}
	}

	// Transport fault toggle edges.
	if prev.TransportFaultActive == 0 && cur.TransportFaultActive == 1 {
		e := base
		e.Type = TypeFaultOn
		out = append(out, e)
	} else if prev.TransportFaultActive == 1 && cur.TransportFaultActive == 0 {
		e := base
		e.Type = TypeFaultOff
		out = append(out, e)
	}

	// player_error transitions to a new non-empty value. The
	// `last_event = 'error'` path lives in snapshot_state.go and
	// catches the explicit marker; this path catches state-change
	// transitions where the player updated player_error without
	// firing a fresh 'error' last_event (legacy SQL's
	// `base WHERE rn>1 AND player_error != '' AND prev_error != player_error`).
	if cur.PlayerError != "" && cur.PlayerError != prev.PlayerError {
		e := base
		e.Type = TypeError
		e.Info = cur.PlayerError
		out = append(out, e)
	}

	return out
}
