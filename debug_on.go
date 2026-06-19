//go:build stubdebug

package main

// debug=true keeps every "Phase N - …" header, the section dumps, and
// the addresses of the loaded-image / initrd / cmdline pointers in the
// stub's serial output. Required by the smoke tests in this repo, which
// grep for "PHASE2-DONE" and friends.
const debug = true
