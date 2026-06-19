// mkesp builds a minimal FAT16 EFI System Partition image containing
// /EFI/BOOT/BOOT<ARCH>.EFI for one or more architectures. It replaces
// the `mtools` invocations (mformat + mmd + mcopy) that the Taskfile
// used to call out to.
//
// We do not use github.com/diskfs/go-diskfs because it only supports
// FAT32, and FAT32 needs ≥ 33 MiB which overflows the 16-bit "sector
// count" field of the El Torito boot record (see trap #6 in the README).
// A 4 MiB FAT16 image fits both constraints — firmware reads it as a
// real FAT16 volume and El Torito can describe it.
//
// Usage:
//
//	mkesp out.img BOOTX64.EFI BOOTAA64.EFI [BOOTRISCV64.EFI] [BOOTLOONGARCH64.EFI] ...
//
// The destination filename inside the ESP is the BASENAME of each input
// path — so pass paths whose final component is the UEFI-spec-mandated
// removable-media fallback name (BOOTX64.EFI, BOOTAA64.EFI,
// BOOTRISCV64.EFI, BOOTLOONGARCH64.EFI). Firmware looks them up by exact
// match on that filename.
//
// The image is exactly 4 MiB. All EFI files together must fit in the
// data area; a `files do not fit` error is raised otherwise.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FAT16 geometry. These constants pick a layout that fits every input
// we care about (up to four ~few-hundred-KiB EFI binaries) in well
// under 4 MiB while still landing inside the FAT16 cluster-count
// window (4085..65524).
const (
	bytesPerSector    = 512
	sectorsPerCluster = 1 // 512 B clusters
	reservedSectors   = 1
	numFATs           = 2
	rootEntries       = 512  // 32 B each → 16 KiB → 32 sectors
	totalSectors      = 8192 // 4 MiB
	sectorsPerFAT     = 32   // 16 KiB FAT16 = 8192 entries
	mediaDesc         = 0xF8 // fixed disk
	volSerial         = 0x12345678
)

// FAT16 directory entry attribute bits.
const (
	attrReadOnly  = 0x01
	attrHidden    = 0x02
	attrSystem    = 0x04
	attrVolumeID  = 0x08
	attrDirectory = 0x10
	attrArchive   = 0x20
)

// entry describes one BOOT<ARCH>.EFI file we are staging.
type entry struct {
	full11   string // the 11-byte FAT short-name (alias if longName is set)
	longName string // empty if 8.3-compatible; else the literal UEFI fallback name
	data     []byte
	clusters uint16 // count
	start    uint16 // first cluster (≥ 4)
}

// dirEntriesCost returns the number of 32-byte directory slots this
// entry consumes — one for the short-name entry, plus one per LFN
// segment (13 UTF-16 chars each) when longName is set.
func (e *entry) dirEntriesCost() int {
	if e.longName == "" {
		return 1
	}
	return 1 + lfnSegmentCount(e.longName)
}

func lfnSegmentCount(name string) int {
	// LFN holds 13 UCS-2 code units per entry, so ceil(len/13).
	utf16len := len([]rune(name)) // ASCII-safe; we only ever see ASCII here
	return (utf16len + 12) / 13
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr,
			"usage: mkesp <out.img> <BOOTX64.EFI> [<BOOTAA64.EFI> <BOOTRISCV64.EFI> ...]")
		os.Exit(2)
	}
	outPath := os.Args[1]

	entries := make([]*entry, 0, len(os.Args)-2)
	for _, p := range os.Args[2:] {
		basename := filepath.Base(p)
		short11, long, err := efiShortName(basename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mkesp: %v\n", err)
			os.Exit(2)
		}
		body, err := os.ReadFile(p)
		must(err)
		entries = append(entries, &entry{full11: short11, longName: long, data: body})
	}

	const clusterSize = sectorsPerCluster * bytesPerSector
	rootSectors := uint32(rootEntries) * 32 / bytesPerSector
	dataOffSec := uint32(reservedSectors) + numFATs*sectorsPerFAT + rootSectors
	clusterCount := (totalSectors - dataOffSec) / sectorsPerCluster

	// Cluster layout (2-indexed because FAT entries 0 and 1 are reserved):
	//   2 → /EFI directory
	//   3 → /EFI/BOOT directory
	//   4..               first input's data
	//   …                 second input's data
	//   ...
	next := uint16(4)
	totalEntryBytes := 0
	for _, e := range entries {
		e.clusters = uint16(clustersNeeded(uint32(len(e.data)), clusterSize))
		e.start = next
		next += e.clusters
		totalEntryBytes += len(e.data)
	}
	if uint32(next) > clusterCount+2 {
		fmt.Fprintf(os.Stderr,
			"mkesp: %d files (total %d B) do not fit in %d-cluster data area\n",
			len(entries), totalEntryBytes, clusterCount)
		os.Exit(1)
	}
	// Each /EFI/BOOT directory entry takes 32 B; "." and ".." use the
	// first 64 B. Long-named entries (BOOTRISCV64.EFI,
	// BOOTLOONGARCH64.EFI) also consume LFN slots — 1 per 13 UTF-16
	// chars — that must fit alongside the short-name entry.
	totalSlots := 0
	for _, e := range entries {
		totalSlots += e.dirEntriesCost()
	}
	bootDirSlots := (clusterSize - 64) / 32 // 64 B reserved for . + ..
	if totalSlots > int(bootDirSlots) {
		fmt.Fprintf(os.Stderr,
			"mkesp: %d entries need %d dir slots, /EFI/BOOT holds %d\n",
			len(entries), totalSlots, bootDirSlots)
		os.Exit(1)
	}

	img := make([]byte, totalSectors*bytesPerSector)

	writeBootSector(img[:bytesPerSector])
	for f := uint32(0); f < numFATs; f++ {
		fatOff := (uint32(reservedSectors) + f*sectorsPerFAT) * bytesPerSector
		writeFAT(img[fatOff:], entries)
	}

	rootOff := (uint32(reservedSectors) + numFATs*sectorsPerFAT) * bytesPerSector
	// Volume label entry first, then /EFI directory entry.
	writeDirEntry(img[rootOff:rootOff+32], "GOCOFFSTUB ", attrVolumeID, 0, 0)
	writeDirEntry(img[rootOff+32:rootOff+64], shortName("EFI", ""), attrDirectory, 2, 0)

	efiDirOff := dataOffSec*bytesPerSector + (2-2)*clusterSize
	writeDirEntry(img[efiDirOff:efiDirOff+32], dotName(), attrDirectory, 2, 0)
	writeDirEntry(img[efiDirOff+32:efiDirOff+64], dotDotName(), attrDirectory, 0, 0)
	writeDirEntry(img[efiDirOff+64:efiDirOff+96], shortName("BOOT", ""), attrDirectory, 3, 0)

	bootDirOff := dataOffSec*bytesPerSector + (3-2)*clusterSize
	writeDirEntry(img[bootDirOff:bootDirOff+32], dotName(), attrDirectory, 3, 0)
	writeDirEntry(img[bootDirOff+32:bootDirOff+64], dotDotName(), attrDirectory, 2, 0)
	cursor := uint32(bootDirOff + 64)
	for _, e := range entries {
		if e.longName != "" {
			n := writeLFNEntries(img[cursor:], e.longName, lfnChecksum(e.full11))
			cursor += uint32(n * 32)
		}
		writeDirEntry(img[cursor:cursor+32], e.full11, 0, e.start, uint32(len(e.data)))
		cursor += 32
	}

	for _, e := range entries {
		dataOff := dataOffSec*bytesPerSector + (uint32(e.start)-2)*clusterSize
		copy(img[dataOff:], e.data)
	}

	must(os.WriteFile(outPath, img, 0o644))
}

func clustersNeeded(size, clusterSize uint32) uint32 {
	if size == 0 {
		return 0
	}
	return (size + clusterSize - 1) / clusterSize
}

// efiShortName turns a filename into a (FAT short name, long name)
// pair. The short name is the on-disk 11-byte 8.3 entry; the long
// name is the literal filename the UEFI firmware searches for — non-
// empty whenever the stem exceeds 8 chars.
//
// Mapping for the supported arches:
//
//	BOOTX64.EFI          → short "BOOTX64 EFI", no LFN needed
//	BOOTAA64.EFI         → short "BOOTAA64EFI", no LFN needed
//	BOOTRISCV64.EFI      → short "BOOTRV64EFI", LFN = "BOOTRISCV64.EFI"
//	BOOTLOONGARCH64.EFI  → short "BOOTLA64EFI", LFN = "BOOTLOONGARCH64.EFI"
//
// UEFI firmware MUST search the long-name table for the literal
// "BOOTRISCV64.EFI" / "BOOTLOONGARCH64.EFI" — the short-name alias
// alone is not enough, since firmware looks for the exact UEFI-spec
// filename. We emit LFN entries for these two arches.
func efiShortName(filename string) (short11, long string, err error) {
	if !strings.EqualFold(filepath.Ext(filename), ".EFI") {
		return "", "", fmt.Errorf("input %q is not a .EFI file", filename)
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(filename, ".efi"), ".EFI")
	if strings.ContainsAny(stem, " /\\") {
		return "", "", fmt.Errorf("input %q has unsupported chars in basename", filename)
	}
	upper := strings.ToUpper(stem)
	switch upper {
	case "BOOTRISCV64":
		return shortName("BOOTRV64", "EFI"), "BOOTRISCV64.EFI", nil
	case "BOOTLOONGARCH64":
		return shortName("BOOTLA64", "EFI"), "BOOTLOONGARCH64.EFI", nil
	}
	if len(upper) > 8 {
		return "", "", fmt.Errorf("basename %q exceeds 8 chars and is not a known UEFI alias", upper)
	}
	return shortName(upper, "EFI"), "", nil
}

// lfnChecksum computes the LFN attached-short-name checksum used by
// every LFN entry. Standard formula, see UEFI 2.10 §13.3 / OSDev
// FAT Long File Names.
func lfnChecksum(short11 string) byte {
	var sum byte
	for i := 0; i < 11; i++ {
		var rotateRight byte
		if sum&1 != 0 {
			rotateRight = 0x80
		}
		sum = rotateRight + (sum >> 1) + short11[i]
	}
	return sum
}

// writeLFNEntries lays down the chain of LFN directory entries for a
// long name, ahead of the short-name entry the caller will write
// next. Entries are stored on disk in REVERSE order (the entry
// containing the tail of the long name comes first), and the entry
// with the highest sequence number has the 0x40 "last" bit set.
//
// Returns the number of 32-byte slots written.
func writeLFNEntries(buf []byte, longName string, checksum byte) int {
	// 13 UCS-2 code units per LFN entry.
	chars := []rune(longName)
	segments := (len(chars) + 12) / 13
	for seg := segments; seg >= 1; seg-- {
		off := (segments - seg) * 32
		ent := buf[off : off+32]

		// Each LFN entry holds chars (seg-1)*13 .. seg*13 of the long
		// name. Positions past the actual end are 0x0000 for the
		// first one (NUL terminator), then 0xFFFF for the rest.
		start := (seg - 1) * 13
		var chunk [13]uint16
		for i := 0; i < 13; i++ {
			idx := start + i
			switch {
			case idx < len(chars):
				chunk[i] = uint16(chars[idx])
			case idx == len(chars):
				chunk[i] = 0x0000
			default:
				chunk[i] = 0xFFFF
			}
		}

		// Sequence number; high bit (0x40) marks the LAST entry (the
		// one written first on disk because LFN entries are reversed).
		ent[0] = byte(seg)
		if seg == segments {
			ent[0] |= 0x40
		}
		// chars 1..5 → bytes 1..10
		for i := 0; i < 5; i++ {
			binary.LittleEndian.PutUint16(ent[1+i*2:1+i*2+2], chunk[i])
		}
		ent[11] = 0x0F // attr: long-name marker
		ent[12] = 0    // type
		ent[13] = checksum
		// chars 6..11 → bytes 14..25
		for i := 0; i < 6; i++ {
			binary.LittleEndian.PutUint16(ent[14+i*2:14+i*2+2], chunk[5+i])
		}
		binary.LittleEndian.PutUint16(ent[26:28], 0) // first cluster (always 0 for LFN)
		// chars 12..13 → bytes 28..31
		binary.LittleEndian.PutUint16(ent[28:30], chunk[11])
		binary.LittleEndian.PutUint16(ent[30:32], chunk[12])
	}
	return segments
}

// shortName builds an 11-byte FAT short name from a base (≤ 8 chars) and
// extension (≤ 3 chars). Both are space-padded to their fixed widths.
func shortName(base, ext string) string {
	if len(base) > 8 || len(ext) > 3 {
		panic("shortName: base/ext too long")
	}
	b := base + "        "
	e := ext + "   "
	return b[:8] + e[:3]
}

// dotName / dotDotName return the special "." and ".." short names used
// in directory clusters. Padded with spaces, not NULs.
func dotName() string    { return "." + "          " }
func dotDotName() string { return ".." + "         " }

func writeBootSector(b []byte) {
	b[0] = 0xEB
	b[1] = 0x3C
	b[2] = 0x90
	copy(b[3:11], []byte("GOCOFF0 ")) // OEM name (8 bytes)
	binary.LittleEndian.PutUint16(b[11:13], bytesPerSector)
	b[13] = sectorsPerCluster
	binary.LittleEndian.PutUint16(b[14:16], reservedSectors)
	b[16] = numFATs
	binary.LittleEndian.PutUint16(b[17:19], rootEntries)
	binary.LittleEndian.PutUint16(b[19:21], totalSectors) // 16-bit total (≤ 65535)
	b[21] = mediaDesc
	binary.LittleEndian.PutUint16(b[22:24], sectorsPerFAT)
	binary.LittleEndian.PutUint16(b[24:26], 63)  // sectors per track (cosmetic)
	binary.LittleEndian.PutUint16(b[26:28], 255) // heads (cosmetic)
	binary.LittleEndian.PutUint32(b[28:32], 0)   // hidden sectors
	binary.LittleEndian.PutUint32(b[32:36], 0)   // total sectors 32-bit (0 because the 16-bit field is non-zero)
	b[36] = 0x80                                 // drive number (HDD)
	b[37] = 0                                    // reserved
	b[38] = 0x29                                 // extended boot signature
	binary.LittleEndian.PutUint32(b[39:43], volSerial)
	copy(b[43:54], []byte("GOCOFFSTUB ")) // volume label (11 bytes)
	copy(b[54:62], []byte("FAT16   "))    // FS type (8 bytes)
	// 62..510: boot code (not used — UEFI ignores it). Leave zero.
	binary.LittleEndian.PutUint16(b[510:512], 0xAA55) // boot sector signature
}

// writeFAT populates one copy of the FAT for an arbitrary number of
// entries. Both FATs are identical so the caller writes this twice
// at the right offsets.
func writeFAT(fat []byte, entries []*entry) {
	// Entry 0 is the media descriptor with all high bits set; entry 1 is
	// the end-of-chain marker that legacy tooling looks at.
	binary.LittleEndian.PutUint16(fat[0:2], 0xFF00|mediaDesc)
	binary.LittleEndian.PutUint16(fat[2:4], 0xFFFF)
	// Cluster 2: /EFI directory (1 cluster) — EOC.
	binary.LittleEndian.PutUint16(fat[4:6], 0xFFFF)
	// Cluster 3: /EFI/BOOT directory (1 cluster) — EOC.
	binary.LittleEndian.PutUint16(fat[6:8], 0xFFFF)

	for _, e := range entries {
		for i := uint16(0); i < e.clusters; i++ {
			cluster := e.start + i
			off := int(cluster) * 2
			var next uint16
			if i == e.clusters-1 {
				next = 0xFFFF
			} else {
				next = cluster + 1
			}
			binary.LittleEndian.PutUint16(fat[off:off+2], next)
		}
	}
}

// writeDirEntry lays down a single 32-byte short-name directory entry.
// We do not bother with timestamps — the firmware does not look at them
// and the build is reproducible.
func writeDirEntry(buf []byte, name11 string, attrs byte, firstCluster uint16, size uint32) {
	if len(name11) != 11 {
		panic("writeDirEntry: name must be exactly 11 bytes")
	}
	copy(buf[0:11], name11)
	buf[11] = attrs
	// 12..21: reserved + timestamps (all zero).
	binary.LittleEndian.PutUint16(buf[20:22], 0) // first cluster high (FAT32 only)
	binary.LittleEndian.PutUint16(buf[26:28], firstCluster)
	binary.LittleEndian.PutUint32(buf[28:32], size)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkesp:", err)
		os.Exit(1)
	}
}
