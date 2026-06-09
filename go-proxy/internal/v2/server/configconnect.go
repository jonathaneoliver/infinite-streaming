package server

// Config-on-connect (#712) bridge into the PATCH-API translator.
//
// The bootstrap/redirect handler in package main parses `proxy.*` URL args
// into a JSON-Merge-Patch-shaped map and needs to project it onto the v1
// SessionData map at session-allocation time. The translation logic already
// lives here (applyPatchToSession + extractPatternSteps), but is unexported
// because the PATCH flow is the only in-package caller. These thin wrappers
// expose exactly what package main needs, so the URL-arg vocabulary rides the
// same translator as the PATCH API and cannot drift from the API model.

// ApplyConfigPatch projects a merge-patch map onto a v1 SessionData map. Pure
// translation — it mutates the map in place and drives no kernel state (that
// is the caller's job, mirroring the PATCH handler's post-write kernel apply).
//
// Called with srv=nil: the only branch that takes a *Server is applyShapePatch,
// which currently ignores it (`_ = srv`); every other branch is srv-free. A
// translate error (e.g. a fault_rule the v1 surface can't express) is returned
// verbatim so the caller can reject the bootstrap request.
func ApplyConfigPatch(sess map[string]any, patch map[string]any) error {
	return applyPatchToSession(nil, sess, patch)
}

// PatternStepsFromSession returns the v1 pattern step slice stashed on the
// session map by a shape.pattern patch, so package main can drive the kernel
// step-engine after materializing config. Returns nil when no pattern is set.
func PatternStepsFromSession(sess map[string]any) []ShapePatternStep {
	return extractPatternSteps(sess)
}
