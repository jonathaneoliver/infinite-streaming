package eventclass

// HTTP status classifier — buckets a single network_requests row by
// status code into http_5xx / http_4xx. Ports the legacy SQL's
// `WHERE status >= 500` / `WHERE status >= 400 AND status < 500`
// UNION branches. Info string matches the legacy
// `concat(toString(status), ' ', method, ' ', path)` format.

func init() {
	RegisterNetwork("network_status", networkStatusClassifier{})
}

type networkStatusClassifier struct{}

func (networkStatusClassifier) ClassifyRequest(req *NetworkRequest) []Event {
	if req == nil {
		return nil
	}
	base := Event{
		Ts: req.Ts, PlayerID: req.PlayerID, PlayID: req.PlayID,
		AttemptID: req.AttemptID, SessionID: req.SessionID,
		Classification: req.Classification,
		Info:           FormatHTTPInfo(req.Status, req.Method, req.Path),
	}
	switch {
	case req.Status >= 500:
		base.Type = TypeHTTP5xx
		return []Event{base}
	case req.Status >= 400:
		base.Type = TypeHTTP4xx
		return []Event{base}
	}
	return nil
}
