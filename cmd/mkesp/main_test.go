package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

// TestEFIShortName covers the four supported UEFI fallback names plus
// invalid input.
func TestEFIShortName(t *testing.T) {
	cases := []struct {
		in        string
		wantShort string
		wantLong  string
		wantErr   bool
	}{
		// 8.3-compatible names — no LFN.
		{"BOOTX64.EFI", "BOOTX64 EFI", "", false},
		{"bootx64.efi", "BOOTX64 EFI", "", false}, // case-insensitive input
		{"BOOTAA64.EFI", "BOOTAA64EFI", "", false},
		{"BOOTIA32.EFI", "BOOTIA32EFI", "", false},
		// Long-named arches — aliased + LFN required.
		{"BOOTRISCV64.EFI", "BOOTRV64EFI", "BOOTRISCV64.EFI", false},
		{"BOOTLOONGARCH64.EFI", "BOOTLA64EFI", "BOOTLOONGARCH64.EFI", false},
		// Error cases.
		{"foo.bin", "", "", true},           // not .EFI
		{"BOOTLONGNAME.EFI", "", "", true},  // long stem with no alias
		{"BOOT X64.EFI", "", "", true},      // space in basename
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			short, long, err := efiShortName(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if short != c.wantShort {
				t.Errorf("short = %q, want %q", short, c.wantShort)
			}
			if long != c.wantLong {
				t.Errorf("long = %q, want %q", long, c.wantLong)
			}
		})
	}
}

// TestLFNChecksum_BOOTRV64EFI pins the checksum against a hand-computed
// value. The standard rotate-right formula for the 11-byte short name
// "BOOTRV64EFI" yields 0x49.
func TestLFNChecksum_BOOTRV64EFI(t *testing.T) {
	got := lfnChecksum("BOOTRV64EFI")
	if got != 0x49 {
		t.Errorf("lfnChecksum(BOOTRV64EFI) = 0x%02x, want 0x49", got)
	}
}

// TestLFNChecksum_StableAcrossKnownShorts captures a few more fixtures
// so refactors of the rotate-right formula can't silently regress.
func TestLFNChecksum_StableAcrossKnownShorts(t *testing.T) {
	cases := []struct {
		in   string
		want byte
	}{
		// Eleven A's: the standard rotate-right formula chained 11 times
		// from 0x00 ends at 0x1C — easy regression marker for changes
		// to the rotate-right path.
		{"AAAAAAAAAAA", 0x1C},
		// Eleven NULs: identity check — no input bytes, no shifting,
		// stays at 0x00.
		{"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00", 0x00},
	}
	for _, c := range cases {
		got := lfnChecksum(c.in)
		if got != c.want {
			t.Errorf("lfnChecksum(%q) = 0x%02x, want 0x%02x", c.in, got, c.want)
		}
	}
}

// TestWriteLFNEntries_RoundTrip writes LFN entries for a known long
// name, then re-parses the bytes back into the long name and checks
// every entry's sequence number, attribute byte, checksum, and the
// reassembled UTF-16 string.
func TestWriteLFNEntries_RoundTrip(t *testing.T) {
	const long = "BOOTRISCV64.EFI"
	const short = "BOOTRV64EFI"
	checksum := lfnChecksum(short)

	buf := make([]byte, 32*lfnSegmentCount(long))
	n := writeLFNEntries(buf, long, checksum)
	if n != 2 {
		t.Fatalf("segments = %d, want 2 (15-char name)", n)
	}

	// Decode each LFN entry.
	// Entries are stored in REVERSE order on disk: entry 0 holds chars
	// 14..15 (with seq=2|0x40), entry 1 holds chars 1..13 (seq=1).
	type lfnEntry struct {
		seq      byte
		attr     byte
		ttype    byte
		checksum byte
		chars    [13]uint16
	}
	parse := func(ent []byte) lfnEntry {
		e := lfnEntry{
			seq:      ent[0],
			attr:     ent[11],
			ttype:    ent[12],
			checksum: ent[13],
		}
		for i := 0; i < 5; i++ {
			e.chars[i] = binary.LittleEndian.Uint16(ent[1+i*2:])
		}
		for i := 0; i < 6; i++ {
			e.chars[5+i] = binary.LittleEndian.Uint16(ent[14+i*2:])
		}
		e.chars[11] = binary.LittleEndian.Uint16(ent[28:])
		e.chars[12] = binary.LittleEndian.Uint16(ent[30:])
		return e
	}

	// First-on-disk entry: chars 14..15 of the long name, seq=2 with 0x40 set.
	e0 := parse(buf[0:32])
	if e0.seq != 0x42 {
		t.Errorf("entry 0 seq = 0x%02x, want 0x42", e0.seq)
	}
	if e0.attr != 0x0F {
		t.Errorf("entry 0 attr = 0x%02x, want 0x0F", e0.attr)
	}
	if e0.checksum != checksum {
		t.Errorf("entry 0 checksum = 0x%02x, want 0x%02x", e0.checksum, checksum)
	}
	if e0.ttype != 0 {
		t.Errorf("entry 0 type = %d, want 0", e0.ttype)
	}
	// Long name has 15 chars; chars 14..15 (0-indexed 13..14) are 'F', 'I'.
	if e0.chars[0] != 'F' || e0.chars[1] != 'I' {
		t.Errorf("entry 0 chars[0..1] = %q,%q, want 'F','I'", e0.chars[0], e0.chars[1])
	}
	// Position right after the name end (index 15 → 0-indexed 15, which is
	// LFN slot index 2 in this segment) should be the 0x0000 NUL terminator.
	if e0.chars[2] != 0x0000 {
		t.Errorf("entry 0 chars[2] = 0x%04x, want 0x0000 (NUL)", e0.chars[2])
	}
	// Positions past the NUL should be 0xFFFF padding.
	for i := 3; i < 13; i++ {
		if e0.chars[i] != 0xFFFF {
			t.Errorf("entry 0 chars[%d] = 0x%04x, want 0xFFFF padding", i, e0.chars[i])
		}
	}

	// Second-on-disk entry: chars 1..13 of the long name, seq=1.
	e1 := parse(buf[32:64])
	if e1.seq != 0x01 {
		t.Errorf("entry 1 seq = 0x%02x, want 0x01", e1.seq)
	}
	if e1.checksum != checksum {
		t.Errorf("entry 1 checksum = 0x%02x, want 0x%02x", e1.checksum, checksum)
	}
	want := []byte("BOOTRISCV64.E") // chars 1..13
	for i, c := range want {
		if e1.chars[i] != uint16(c) {
			t.Errorf("entry 1 chars[%d] = 0x%04x (%q), want 0x%04x (%q)",
				i, e1.chars[i], rune(e1.chars[i]), c, rune(c))
		}
	}
}

// TestEntry_DirEntriesCost gates the cluster-capacity check.
func TestEntry_DirEntriesCost(t *testing.T) {
	cases := []struct {
		short, long string
		want        int
	}{
		{"BOOTX64 EFI", "", 1},                          // 8.3-compatible
		{"BOOTRV64EFI", "BOOTRISCV64.EFI", 1 + 2},       // 15-char LFN → 2 segs
		{"BOOTLA64EFI", "BOOTLOONGARCH64.EFI", 1 + 2},   // 19-char LFN → 2 segs
	}
	for _, c := range cases {
		e := &entry{full11: c.short, longName: c.long}
		if got := e.dirEntriesCost(); got != c.want {
			t.Errorf("dirEntriesCost(%q/%q) = %d, want %d", c.short, c.long, got, c.want)
		}
	}
}

// TestLFNSegmentCount covers the boundary at 13 chars.
func TestLFNSegmentCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"A", 1},
		{strings.Repeat("X", 13), 1}, // exactly 13 → one segment
		{strings.Repeat("X", 14), 2}, // 14 chars → two segments
		{strings.Repeat("X", 26), 2}, // exactly 26 → two segments
		{strings.Repeat("X", 27), 3},
	}
	for _, c := range cases {
		if got := lfnSegmentCount(c.in); got != c.want {
			t.Errorf("lfnSegmentCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
