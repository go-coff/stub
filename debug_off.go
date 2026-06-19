//go:build !stubdebug

package main

// debug controls whether the stub prints its phase-by-phase trace
// (section dumps, addresses, "Phase 3a - …" headers, etc.). Default
// build = silent: the boot log gets only the kernel's own output plus
// hard-error messages from the stub.
//
// To re-enable the verbose trace for a debugging session, rebuild with
//
//	tinygo build -tags stubdebug …
//
// — see debug_on.go for the matching const.
const debug = false
