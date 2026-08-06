//go:build !linux

package bootstrap

// FreeBytes reports -1 everywhere the daemon does not ship.
//
// Forktower runs in a Linux container on every platform it is packaged for, so
// this exists to keep the package buildable on a maintainer's own machine rather
// than to support running there. Unknown rather than a guess: -1 makes the plan
// skip the disk check instead of refusing on a number nobody measured.
func FreeBytes(string) int64 { return -1 }
