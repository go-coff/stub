// mkiso builds a hybrid ISO 9660 image with an El Torito UEFI boot
// record pointing to an embedded ESP. It replaces the `xorriso`
// invocation that the Taskfile used to call out to.
//
// Pipeline:
//
//   workspace/
//   ├── EFI/BOOT/BOOTX64.EFI         visible to any tool that mounts the ISO
//   ├── EFI/BOOT/BOOTAA64.EFI        "
//   ├── EFI/BOOT/BOOTRISCV64.EFI     "
//   ├── EFI/BOOT/BOOTLOONGARCH64.EFI "
//   └── boot/efi.img                 the FAT16 ESP; El Torito UEFI boot
//                                    record points here, so firmware reading
//                                    the CD uses this file as the boot volume.
//
// All inputs are passed in on the command line; we stage them in a
// temp workspace and hand it to github.com/diskfs/go-diskfs to render
// the ISO.
//
// Usage:
//
//	mkiso out.iso esp.img BOOTX64.EFI [BOOTAA64.EFI BOOTRISCV64.EFI ...]
//
// The destination filename inside the ISO mirrors the BASENAME of
// each input path.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr,
			"usage: mkiso <out.iso> <esp.img> <BOOTX64.EFI> [<BOOTAA64.EFI> <BOOTRISCV64.EFI> ...]")
		os.Exit(2)
	}
	outPath := os.Args[1]
	espPath := os.Args[2]
	ukiPaths := os.Args[3:]

	workspace, err := os.MkdirTemp("", "mkiso-")
	must(err)
	defer os.RemoveAll(workspace)

	// Stage the ISO9660 tree. The El Torito UEFI boot file lives at
	// /boot/efi.img; firmware loads that as the boot volume. The
	// BOOT<ARCH>.EFI copies under /EFI/BOOT/ are not strictly required
	// for boot (firmware reads from the El Torito-pointed efi.img),
	// but they make the ISO usable as a loopback-mounted source on a
	// running OS.
	stage := func(src, relDest string) {
		dst := filepath.Join(workspace, relDest)
		must(os.MkdirAll(filepath.Dir(dst), 0o755))
		in, err := os.Open(src)
		must(err)
		defer in.Close()
		out, err := os.Create(dst)
		must(err)
		defer out.Close()
		_, err = io.Copy(out, in)
		must(err)
	}
	stage(espPath, "boot/efi.img")
	for _, p := range ukiPaths {
		stage(p, "EFI/BOOT/"+filepath.Base(p))
	}

	// Pre-create the output file; the backend wants a real *os.File.
	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		must(err)
	}
	f, err := os.Create(outPath)
	must(err)
	defer f.Close()

	bk := file.New(f, false)
	fs, err := iso9660.Create(bk, 0, 0, 2048, workspace)
	must(err)

	err = fs.Finalize(iso9660.FinalizeOptions{
		RockRidge:        true,
		VolumeIdentifier: "GOCOFFSTUB",
		ElTorito: &iso9660.ElTorito{
			BootCatalog: "/boot.catalog",
			Entries: []*iso9660.ElToritoEntry{
				{
					Platform:  iso9660.EFI,
					Emulation: iso9660.NoEmulation,
					BootFile:  "/boot/efi.img",
				},
			},
		},
	})
	must(err)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkiso:", err)
		os.Exit(1)
	}
}
