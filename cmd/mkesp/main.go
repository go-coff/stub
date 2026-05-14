// mkesp builds a minimal FAT16 EFI System Partition image containing
// /EFI/BOOT/BOOTX64.EFI and /EFI/BOOT/BOOTAA64.EFI. It replaces the
// `mtools` invocations (mformat + mmd + mcopy) that the Taskfile used
// to call out to.
//
// We do not use github.com/diskfs/go-diskfs because it only supports
// FAT32, and FAT32 needs ≥ 33 MiB which overflows the 16-bit "sector
// count" field of the El Torito boot record (see trap #6 in the README).
// A 4 MiB FAT16 image fits both constraints — firmware reads it as a
// real FAT16 volume and El Torito can describe it.
//
// Usage:
//
//	mkesp out.img BOOTX64.EFI BOOTAA64.EFI
//
// The image is exactly 4 MiB. All EFI files must fit in the data area;
// a `files do not fit` error is raised otherwise.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// FAT16 geometry. These constants pick a layout that fits every input
// we care about (two ~few-KiB EFI binaries) in well under 4 MiB while
// still landing inside the FAT16 cluster-count window (4085..65524).
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

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: mkesp <out.img> <BOOTX64.EFI> <BOOTAA64.EFI>")
		os.Exit(2)
	}
	outPath := os.Args[1]
	x64, err := os.ReadFile(os.Args[2])
	must(err)
	aa64, err := os.ReadFile(os.Args[3])
	must(err)

	const clusterSize = sectorsPerCluster * bytesPerSector
	rootSectors := uint32(rootEntries) * 32 / bytesPerSector
	dataOffSec := uint32(reservedSectors) + numFATs*sectorsPerFAT + rootSectors
	clusterCount := (totalSectors - dataOffSec) / sectorsPerCluster

	// Cluster layout (2-indexed because FAT entries 0 and 1 are reserved):
	//   2 → /EFI directory
	//   3 → /EFI/BOOT directory
	//   4..              BOOTX64.EFI data
	//   4 + x64Clusters..BOOTAA64.EFI data
	x64Clusters := clustersNeeded(uint32(len(x64)), clusterSize)
	aa64Clusters := clustersNeeded(uint32(len(aa64)), clusterSize)
	x64Start := uint16(4)
	aa64Start := uint16(4) + uint16(x64Clusters)
	if uint32(aa64Start)+aa64Clusters > clusterCount+2 {
		fmt.Fprintf(os.Stderr, "mkesp: files (x64=%d, aa64=%d) do not fit in %d-cluster data area\n",
			len(x64), len(aa64), clusterCount)
		os.Exit(1)
	}

	img := make([]byte, totalSectors*bytesPerSector)

	writeBootSector(img[:bytesPerSector])
	for f := uint32(0); f < numFATs; f++ {
		fatOff := (uint32(reservedSectors) + f*sectorsPerFAT) * bytesPerSector
		writeFAT(img[fatOff:], x64Start, uint16(x64Clusters), aa64Start, uint16(aa64Clusters))
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
	writeDirEntry(img[bootDirOff+64:bootDirOff+96],
		shortName("BOOTX64", "EFI"), 0, x64Start, uint32(len(x64)))
	writeDirEntry(img[bootDirOff+96:bootDirOff+128],
		shortName("BOOTAA64", "EFI"), 0, aa64Start, uint32(len(aa64)))

	x64DataOff := dataOffSec*bytesPerSector + (uint32(x64Start)-2)*clusterSize
	copy(img[x64DataOff:], x64)
	aa64DataOff := dataOffSec*bytesPerSector + (uint32(aa64Start)-2)*clusterSize
	copy(img[aa64DataOff:], aa64)

	must(os.WriteFile(outPath, img, 0o644))
}

func clustersNeeded(size, clusterSize uint32) uint32 {
	if size == 0 {
		return 0
	}
	return (size + clusterSize - 1) / clusterSize
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

// writeFAT populates one copy of the FAT. Both FATs are identical so
// the caller writes this twice at the right offsets.
func writeFAT(fat []byte, x64Start, x64Clusters, aa64Start, aa64Clusters uint16) {
	// Entry 0 is the media descriptor with all high bits set; entry 1 is
	// the end-of-chain marker that legacy tooling looks at.
	binary.LittleEndian.PutUint16(fat[0:2], 0xFF00|mediaDesc)
	binary.LittleEndian.PutUint16(fat[2:4], 0xFFFF)
	// Cluster 2: /EFI directory (1 cluster) — EOC.
	binary.LittleEndian.PutUint16(fat[4:6], 0xFFFF)
	// Cluster 3: /EFI/BOOT directory (1 cluster) — EOC.
	binary.LittleEndian.PutUint16(fat[6:8], 0xFFFF)

	chain := func(start, count uint16) {
		for i := uint16(0); i < count; i++ {
			cluster := start + i
			off := int(cluster) * 2
			var next uint16
			if i == count-1 {
				next = 0xFFFF
			} else {
				next = cluster + 1
			}
			binary.LittleEndian.PutUint16(fat[off:off+2], next)
		}
	}
	chain(x64Start, x64Clusters)
	chain(aa64Start, aa64Clusters)
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
	binary.LittleEndian.PutUint16(buf[20:22], 0)         // first cluster high (FAT32 only)
	binary.LittleEndian.PutUint16(buf[26:28], firstCluster)
	binary.LittleEndian.PutUint32(buf[28:32], size)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkesp:", err)
		os.Exit(1)
	}
}
