//go:build !linux

package main

import "time"

// pingSocket / newPingSocket on non-Linux platforms are stubs so the
// dev build (typically macOS) keeps compiling. Whole feature is gated
// at the sampler-start path — newPingSocket returns (nil, nil) and
// the sampler logs "disabled" and exits. Production go-proxy only
// runs in the Linux container.
type pingSocket struct{}

func newPingSocket() (*pingSocket, error) {
	return nil, nil
}

func (s *pingSocket) ping(addr string, timeout time.Duration) (uint32, error) {
	return 0, nil
}
