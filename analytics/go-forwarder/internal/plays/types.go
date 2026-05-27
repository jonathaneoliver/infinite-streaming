package plays

// PlayFilter is the typed parameter set every list/find operation
// takes. Empty fields = no constraint.
//
// IDs (PlayerID, PlayID) are the caller's responsibility to
// canonicalise — the handlers do it at the HTTP boundary via the
// main package's canonicalV2ID; chat tools do it at tool entry. The
// domain layer just passes them through to lowerUTF8-comparing WHERE
// clauses, which makes the case-fold idempotent anyway.
type PlayFilter struct {
	PlayerID       string
	PlayID         string
	AttemptID      string
	SessionID      string
	From           string // CH datetime string; parsed via parseDateTime64BestEffort
	To             string
	Classification string // "interesting" | "other" | "favourite" — validated
	Labels         LabelFilter

	// Limit caps the returned rows. Zero means "use the call's default."
	// FindPlays caps at 5000 regardless to protect CH.
	Limit int
}

// defaultLimit returns the per-call default when the caller didn't
// supply one. Kept tiny so callers see this constant rather than
// hunting for it inside the function bodies.
const defaultPlaysLimit = 500
const maxPlaysLimit = 5000
