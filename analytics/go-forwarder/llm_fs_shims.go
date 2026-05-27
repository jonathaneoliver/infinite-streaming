package main

// Thin os.* aliases used by saveFinding (and any future write-side
// tools). Kept in one place so future swap-for-mocked-fs in tests
// is a single import change.

import "os"

var (
	osStat      = os.Stat
	osMkdirAll  = os.MkdirAll
	osWriteFile = os.WriteFile
)
