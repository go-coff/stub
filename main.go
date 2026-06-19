// Command stub is a UEFI Unified Kernel Image stub written in TinyGo.
//
// Phase 1 — print a banner via SimpleTextOutput.
// Phase 2 — locate our own PE image at runtime via EFI_LOADED_IMAGE_PROTOCOL
//          and walk the embedded section table. This is the runtime equivalent
//          of what `pe.Append` does at build time: it gives the stub a way to
//          read its own `.linux` / `.initrd` / `.cmdline` / `.osrel` payloads.
// Phase 3 — kernel handoff via the EFI handover protocol (TODO).
//
// All EFI method slots are declared as `uintptr` so the Go struct layout
// matches the firmware-side C layout byte-for-byte (a Go `func` value is a
// fat pointer and would shift every subsequent field by 8 bytes). The
// indirect calls go through asm thunks in thunk-{amd64,arm64}.S that translate
// from TinyGo's external-call register layout to the Microsoft x64 / AAPCS64
// calling convention.
package main

import "unsafe"

// ----- EFI primitive types -----

// efiStatus is UINTN (pointer-width). On amd64 / aarch64 it is uint64.
type efiStatus = uint64

const efiSuccess efiStatus = 0

// efiGUID is the 16-byte UEFI/Microsoft GUID layout: Data1 (LE u32),
// Data2 (LE u16), Data3 (LE u16), then 8 bytes in their natural order.
type efiGUID [16]byte

// efiTableHeader (24 bytes) leads every standard EFI table.
type efiTableHeader struct {
	signature  uint64
	revision   uint32
	headerSize uint32
	crc32      uint32
	_reserved  uint32
}

// ----- EFI protocols -----

// EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL.
type efiSimpleTextOutput struct {
	reset        uintptr // 0x00
	outputString uintptr // 0x08  EFI_TEXT_STRING
	testString   uintptr // 0x10
	queryMode    uintptr // 0x18
	setMode      uintptr // 0x20
	setAttribute uintptr // 0x28
	clearScreen  uintptr // 0x30
	setCursorPos uintptr // 0x38
	enableCursor uintptr // 0x40
	mode         uintptr // 0x48
}

// efiBootServices — the prefix down to HandleProtocol, which is all phase 2
// needs. Layout below is taken straight from the UEFI 2.10 specification.
type efiBootServices struct {
	hdr efiTableHeader // 0x00 (24 bytes)

	// Task Priority Services.
	raiseTPL   uintptr // 0x18
	restoreTPL uintptr // 0x20

	// Memory Services.
	allocatePages uintptr // 0x28
	freePages     uintptr // 0x30
	getMemoryMap  uintptr // 0x38
	allocatePool  uintptr // 0x40
	freePool      uintptr // 0x48

	// Event & Timer Services.
	createEvent  uintptr // 0x50
	setTimer     uintptr // 0x58
	waitForEvent uintptr // 0x60
	signalEvent  uintptr // 0x68
	closeEvent   uintptr // 0x70
	checkEvent   uintptr // 0x78

	// Protocol Handler Services.
	installProtocolInterface   uintptr // 0x80
	reinstallProtocolInterface uintptr // 0x88
	uninstallProtocolInterface uintptr // 0x90
	handleProtocol             uintptr // 0x98  ← phase-2 entry point
	_reserved                  uintptr // 0xA0
	registerProtocolNotify     uintptr // 0xA8
	locateHandle               uintptr // 0xB0
	locateDevicePath           uintptr // 0xB8
	installConfigurationTable  uintptr // 0xC0

	// Image Services.
	loadImage  uintptr // 0xC8  ← phase-3 entry point
	startImage uintptr // 0xD0
	// Truncated past here; firmware fills the rest in but we never read
	// it, so leaving the struct short is harmless.
}

// EFI_DEVICE_PATH_PROTOCOL header (4 bytes). A device path is a chain of
// these nodes terminated by an "end of hardware device path" node
// (Type=0x7F, SubType=0xFF, Length=4).
type efiDevicePathHeader struct {
	dpType    uint8
	dpSubType uint8
	dpLength  uint16 // little-endian, includes the header
}

// vendorMediaInitrdPath is the device path Linux ≥ 5.7 looks up to find
// an embedded initrd. Layout:
//
//	[0:4]   header (Type=0x04 MEDIA_DEVICE_PATH, SubType=0x03 VENDOR, Length=20)
//	[4:20]  Linux initrd vendor GUID 5568e427-68fc-4f3d-ac74-ca555231cc68
//	[20:24] end-of-path header (Type=0x7F, SubType=0xFF, Length=4)
//
// The whole blob lives in BSS so its address is stable across the call to
// InstallProtocolInterface — same escape-analysis rule as the other
// firmware-facing buffers.
var vendorMediaInitrdPath [24]uint8

// initrdDataPtr and initrdSize back the LoadFile2 callback. They are
// populated in _start once `.initrd` is located, and read directly by
// `goLoadFile2` below. Pure Go vars — earlier attempts tried to share
// them with an asm-side callback via //go:linkname, but the dual
// definition (Go BSS + asm BSS) ended up at different addresses in
// the merged image so the asm read zeros.
var initrdDataPtr uintptr
var initrdSize uint64

// loadFile2Ptr returns the runtime address of the LoadFile2 callback
// implemented in asm. Implemented PC-relative in the thunks (adrp / lea
// rip-relative) so no `.reloc` fixup is involved — that matters because
// `pec.Append` does not regenerate the base-relocation table, which
// previously caused our function pointer to come out zero at boot.
//
//go:linkname loadFile2Ptr loadFile2Ptr
func loadFile2Ptr() uintptr

// initrdHandle is the OUT slot for InstallProtocolInterface — it
// receives a fresh handle that owns our LoadFile2 + DevicePath
// protocols. Must be BSS (escape-analysis rule again).
var initrdHandle uintptr

// EFI_LOAD_FILE2_PROTOCOL — a single LoadFile entry point. Spec:
//
//	EFI_STATUS LoadFile(this, devPath, bootPolicy, bufSize *UINTN, buf VOID*);
//
// The slot is a uintptr (not `func(...)`) so the struct layout matches
// the C definition byte-for-byte.
type efiLoadFile2Protocol struct {
	loadFile uintptr
}

// ourLoadFile2 is the protocol instance the firmware sees. Lives in BSS
// so its address is stable and so the installed pointer keeps resolving
// after _start has run.
var ourLoadFile2 efiLoadFile2Protocol

// EFI_LOADED_IMAGE_PROTOCOL — gives us our own image's base address and
// size in memory once the firmware has loaded us.
type efiLoadedImageProtocol struct {
	revision        uint32  // 0x00
	_pad            uint32  // 0x04
	parentHandle    uintptr // 0x08
	systemTable     uintptr // 0x10
	deviceHandle    uintptr // 0x18
	filePath        uintptr // 0x20
	_reserved       uintptr // 0x28
	loadOptionsSize uint32  // 0x30
	_pad2           uint32  // 0x34
	loadOptions     uintptr // 0x38
	imageBase       uintptr // 0x40  ← pointer to the DOS header of our PE
	imageSize       uint64  // 0x48  ← length in bytes of the image in memory
	imageCodeType   uint32  // 0x50
	imageDataType   uint32  // 0x54
	unload          uintptr // 0x58
}

// EFI_SYSTEM_TABLE — fleshed out enough to reach BootServices.
type efiSystemTable struct {
	hdr                  efiTableHeader       // 0x00, 24 bytes
	firmwareVendor       *uint16              // 0x18
	firmwareRevision     uint32               // 0x20
	_pad1                uint32               // 0x24 (alignment)
	consoleInHandle      uintptr              // 0x28
	conIn                uintptr              // 0x30
	consoleOutHandle     uintptr              // 0x38
	conOut               *efiSimpleTextOutput // 0x40
	standardErrorHandle  uintptr              // 0x48
	stdErr               uintptr              // 0x50
	runtimeServices      uintptr              // 0x58
	bootServices         *efiBootServices     // 0x60
	numberOfTableEntries uintptr              // 0x68
	configurationTable   uintptr              // 0x70
}

// ----- EFI GUIDs -----

// EFI_LOADED_IMAGE_PROTOCOL_GUID: 5B1B31A1-9562-11D2-8E3F-00A0C969723B.
// Data1/Data2/Data3 are little-endian; Data4 is laid out in natural order.
var loadedImageGUID = efiGUID{
	0xA1, 0x31, 0x1B, 0x5B,
	0x62, 0x95,
	0xD2, 0x11,
	0x8E, 0x3F, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B,
}

// EFI_LOAD_FILE2_PROTOCOL_GUID: 4006C0C1-FCB3-403E-996D-4A6C8724E06D.
var loadFile2GUID = efiGUID{
	0xC1, 0xC0, 0x06, 0x40,
	0xB3, 0xFC,
	0x3E, 0x40,
	0x99, 0x6D, 0x4A, 0x6C, 0x87, 0x24, 0xE0, 0x6D,
}

// EFI_DEVICE_PATH_PROTOCOL_GUID: 09576E91-6D3F-11D2-8E39-00A0C969723B.
var devicePathGUID = efiGUID{
	0x91, 0x6E, 0x57, 0x09,
	0x3F, 0x6D,
	0xD2, 0x11,
	0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B,
}

// LINUX_EFI_INITRD_MEDIA_GUID: 5568E427-68FC-4F3D-AC74-CA555231CC68.
// The Linux kernel ≥ 5.7 walks every UEFI handle that exposes a media
// vendor device path with this GUID and, if it finds one, calls the
// associated LoadFile2 protocol to pull its initrd out.
var linuxInitrdGUID = efiGUID{
	0x27, 0xE4, 0x68, 0x55,
	0xFC, 0x68,
	0x3D, 0x4F,
	0xAC, 0x74, 0xCA, 0x55, 0x52, 0x31, 0xCC, 0x68,
}

// ----- Thunks (see thunk-{amd64,arm64}.S) -----

//go:linkname efiCall1 efiCall1
func efiCall1(fn, a uintptr) uint64

//go:linkname efiCall2 efiCall2
func efiCall2(fn, a, b uintptr) uint64

//go:linkname efiCall3 efiCall3
func efiCall3(fn, a, b, c uintptr) uint64

//go:linkname efiCall4 efiCall4
func efiCall4(fn, a, b, c, d uintptr) uint64

//go:linkname efiCall5 efiCall5
func efiCall5(fn, a, b, c, d, e uintptr) uint64

//go:linkname efiCall6 efiCall6
func efiCall6(fn, a, b, c, d, e, f uintptr) uint64

// ----- Print helpers -----
//
// Everything goes through a single per-call scratch buffer in BSS. We
// never allocate on the print path — TinyGo's "leaking" GC has no heap
// configured for us, so allocation would crash on the first call.

var lineBuf [128]uint16

// loadedImageHolder is the OUT slot HandleProtocol writes through. It
// MUST be package-level (BSS): if it were a local, TinyGo's escape
// analysis would see `&loadedImageHolder` and emit a call to
// runtime.alloc() — which on a freestanding `gc: leaking` build with
// no heap initialised resolves to garbage and ruins everything before
// we even reach the firmware.
var loadedImageHolder uintptr

// nameBuf holds the (up to 8) ASCII bytes of a PE section name widened
// to UTF-16, with a trailing NUL. Same escape-analysis rationale as
// loadedImageHolder above: a stack-local would have to be heap-allocated
// the moment `&nameBuf[0]` flows into an external thunk call, and that
// path doesn't survive a freestanding `gc: leaking` build.
var nameBuf [9]uint16

// Phase 3 state. dotLinuxVA / dotLinuxSize are populated by walkSections
// when it spots a `.linux` section; if both stay zero, we skip the kernel
// handoff and just spin (the unloaded leaf case). childImageHandle is
// the OUT slot LoadImage writes through — has to live in BSS for the
// same reason as loadedImageHolder (trap #8).
var (
	dotLinuxVA       uint32
	dotLinuxSize     uint32
	childImageHandle uintptr
)

// Phase 3c state. The child kernel reads its command line through its own
// EFI_LOADED_IMAGE_PROTOCOL.LoadOptions, a NUL-terminated UTF-16LE buffer
// owned by us — the firmware does not copy it. cmdlineUTF16 therefore must
// live in BSS so the pointer stays valid past StartImage / ExitBootServices,
// and so HandleProtocol's OUT slot (childLIPHolder) does not get
// heap-allocated by TinyGo's escape analysis.
//
// 1024 UTF-16 code units = 2048 bytes — comfortably above the typical
// kernel cmdline cap (CONFIG_CMDLINE_SIZE defaults to 256 on x86_64 and
// 2048 on arm64). Longer cmdlines are silently truncated rather than
// aborted: a too-long cmdline is a build-time issue, not worth blocking
// the boot for.
var (
	childLIPHolder uintptr
	cmdlineUTF16   [1024]uint16
)

// ukiSection records where one of the named UKI payload sections lives
// inside our loaded image. `vaddr` is relative to imageBase: the bytes
// of the section are accessible at `imageBase + vaddr` for `vsize` bytes.
type ukiSection struct {
	vaddr uint32
	vsize uint32
	found bool
}

// The five UKI payload sections that systemd-stub recognises. Same names
// the companion `pec` CLI uses when adding sections at build time. All
// live in BSS — same escape-analysis rule as the other holders above:
// once we capture an `&secCmdline` to read its body, the variable must
// not be a function-local.
var (
	secLinux   ukiSection
	secInitrd  ukiSection
	secCmdline ukiSection
	secOsrel   ukiSection
	secUname   ukiSection
)

// writeUTF16 prints a NUL-terminated UTF-16LE buffer to ConOut.
func writeUTF16(co *efiSimpleTextOutput, p *uint16) {
	efiCall2(co.outputString,
		uintptr(unsafe.Pointer(co)),
		uintptr(unsafe.Pointer(p)))
}

// writeASCII widens a Go (ASCII / Latin-1) string into UTF-16 in lineBuf
// and prints it. The buffer is shared, so writeASCII must finish writing
// before another writeASCII / writeHex / writeDec is called — which is
// always the case on the single-threaded firmware boot path.
func writeASCII(co *efiSimpleTextOutput, s string) {
	n := 0
	for i := 0; i < len(s) && n < len(lineBuf)-1; i++ {
		lineBuf[n] = uint16(s[i])
		n++
	}
	lineBuf[n] = 0
	writeUTF16(co, &lineBuf[0])
}

// dbg prints s only when the `stubdebug` build tag is set (see
// debug_off.go / debug_on.go). With debug=false the body is dead code
// the compiler strips, so a quiet build pays zero runtime cost for the
// hundreds of phase-trace messages scattered through _start.
func dbg(co *efiSimpleTextOutput, s string) {
	if debug {
		writeASCII(co, s)
	}
}

// dbgHex64 is the writeHex64 counterpart of dbg — gated identically so a
// quiet build never emits an address or size.
func dbgHex64(co *efiSimpleTextOutput, n uint64) {
	if debug {
		writeHex64(co, n)
	}
}

// dbgDec and dbgUTF16 mirror dbgHex64 / dbg for the two remaining
// formatting primitives. Same gate, same zero-cost-when-quiet rationale.
func dbgDec(co *efiSimpleTextOutput, n uint32) {
	if debug {
		writeDec(co, n)
	}
}

func dbgUTF16(co *efiSimpleTextOutput, p *uint16) {
	if debug {
		writeUTF16(co, p)
	}
}

// writeHex64 prints "0x" + 16 hex digits (zero-padded) for a uint64.
func writeHex64(co *efiSimpleTextOutput, n uint64) {
	lineBuf[0] = '0'
	lineBuf[1] = 'x'
	for i := 0; i < 16; i++ {
		nib := uint8((n >> (60 - 4*i)) & 0xF)
		if nib < 10 {
			lineBuf[2+i] = uint16('0' + nib)
		} else {
			lineBuf[2+i] = uint16('a' + nib - 10)
		}
	}
	lineBuf[18] = 0
	writeUTF16(co, &lineBuf[0])
}

// writeDec prints a uint32 in decimal (no prefix). Buffer length is
// fixed at 10 digits which is enough for any uint32.
func writeDec(co *efiSimpleTextOutput, n uint32) {
	// Build the digits backwards into a temporary, then copy reversed.
	var tmp [10]uint8
	i := 0
	if n == 0 {
		lineBuf[0] = '0'
		lineBuf[1] = 0
		writeUTF16(co, &lineBuf[0])
		return
	}
	for n > 0 {
		tmp[i] = uint8(n % 10)
		n /= 10
		i++
	}
	for j := 0; j < i; j++ {
		lineBuf[j] = uint16('0' + tmp[i-1-j])
	}
	lineBuf[i] = 0
	writeUTF16(co, &lineBuf[0])
}

// ----- PE walking -----

// peU16 reads a little-endian uint16 at `base+off` from in-memory PE.
func peU16(base, off uintptr) uint16 {
	return *(*uint16)(unsafe.Pointer(base + off))
}

// peU32 reads a little-endian uint32 at `base+off`.
// peByte reads a single byte at `base+off`. Used to walk the body of an
// ASCII UKI section without a slice (no allocation on a freestanding
// build) and without bounds-checking overhead.
func peByte(base, off uintptr) byte {
	return *(*byte)(unsafe.Pointer(base + off))
}

// secNameIs returns true when the 8-byte PE section name at `entry`
// matches `target` (≤ 8 chars, the rest of the field must be NUL-padded).
// Comparing the raw bytes lets us avoid widening the section name into
// nameBuf just for the match.
func secNameIs(entry uintptr, target string) bool {
	if len(target) > 8 {
		return false
	}
	for i := 0; i < len(target); i++ {
		if peByte(entry, uintptr(i)) != target[i] {
			return false
		}
	}
	// Anything past `target` in the 8-byte field must be NUL — otherwise
	// e.g. ".linuxX" would match a target of ".linux".
	for i := len(target); i < 8; i++ {
		if peByte(entry, uintptr(i)) != 0 {
			return false
		}
	}
	return true
}

func peU32(base, off uintptr) uint32 {
	return *(*uint32)(unsafe.Pointer(base + off))
}

// walkSections prints the section table embedded inside the PE that
// starts at `base`. The layout we walk is exactly what `pe.Append`
// constructs at build time, so any UKI payload section (`.linux`,
// `.initrd`, `.cmdline`, `.osrel`, `.uname`) injected by `pec` shows up
// here verbatim.
func walkSections(co *efiSimpleTextOutput, base uintptr) {
	// DOS header: e_lfanew at offset 0x3C points at the PE header.
	peOff := uintptr(peU32(base, 0x3C))
	pe := base + peOff

	// PE signature ("PE\0\0") is 4 bytes; COFF File Header follows.
	coff := pe + 4
	numSections := peU16(coff, 2)
	sizeOfOpt := peU16(coff, 16)

	// Section table sits right after the optional header.
	secTable := coff + 20 + uintptr(sizeOfOpt)

	dbg(co, "  sections: ")
	dbgDec(co, uint32(numSections))
	dbg(co, "\r\n")

	for i := uint16(0); i < numSections; i++ {
		s := secTable + uintptr(i)*40

		// Name field is 8 bytes of ASCII, NUL-padded. Use a package-level
		// buffer; see nameBuf for the escape-analysis rationale.
		nameBuf[8] = 0
		for j := uintptr(0); j < 8; j++ {
			c := *(*uint8)(unsafe.Pointer(s + j))
			nameBuf[j] = uint16(c)
			if c == 0 {
				break
			}
		}

		// virtualSize at +8, virtualAddress at +12.
		vsize := peU32(s, 8)
		vaddr := peU32(s, 12)

		dbg(co, "    ")
		dbgUTF16(co, &nameBuf[0])
		dbg(co, "  va=")
		dbgHex64(co, uint64(vaddr))
		dbg(co, "  size=")
		dbgHex64(co, uint64(vsize))
		dbg(co, "\r\n")

		// Record `.linux` for the phase-3 kernel handoff. Match the
		// 8-byte field in nameBuf rather than building a Go string —
		// we have no allocator. Section names are short ASCII so an
		// open-coded compare is cheaper than anything generic.
		if nameBuf[0] == '.' && nameBuf[1] == 'l' && nameBuf[2] == 'i' &&
			nameBuf[3] == 'n' && nameBuf[4] == 'u' && nameBuf[5] == 'x' &&
			nameBuf[6] == 0 {
			dotLinuxVA = vaddr
			dotLinuxSize = vsize
		}

		// Capture every UKI payload section we recognise (phase 3a). The
		// five names below are the systemd-stub PE annex contract — same
		// names the companion `pec` CLI writes at build time. Done via
		// `secNameIs` against the raw 8-byte name field, no allocation.
		switch {
		case secNameIs(s, ".linux"):
			secLinux = ukiSection{vaddr, vsize, true}
		case secNameIs(s, ".initrd"):
			secInitrd = ukiSection{vaddr, vsize, true}
		case secNameIs(s, ".cmdline"):
			secCmdline = ukiSection{vaddr, vsize, true}
		case secNameIs(s, ".osrel"):
			secOsrel = ukiSection{vaddr, vsize, true}
		case secNameIs(s, ".uname"):
			secUname = ukiSection{vaddr, vsize, true}
		}
	}
}

// reportUKISection prints "<name>: <state>" for one of the five UKI
// payload sections. The state is either "missing" or "va=… size=…".
func reportUKISection(co *efiSimpleTextOutput, name string, s ukiSection) {
	dbg(co, "    ")
	dbg(co, name)
	if !s.found {
		dbg(co, ": missing\r\n")
		return
	}
	dbg(co, ": va=")
	dbgHex64(co, uint64(s.vaddr))
	dbg(co, " size=")
	dbgHex64(co, uint64(s.vsize))
	dbg(co, "\r\n")
}

// dumpASCIISection prints up to lineBuf-4 wide chars of `s`'s body,
// replacing control characters with '.' so a stray CR/LF inside `.osrel`
// does not scramble the terminal. Stops at the first NUL.
func dumpASCIISection(co *efiSimpleTextOutput, imageBase uintptr, s ukiSection) {
	if !s.found || s.vsize == 0 {
		return
	}
	max := uint32(len(lineBuf)) - 4 // room for `...` + trailing NUL
	n := s.vsize
	truncated := false
	if n > max {
		n = max
		truncated = true
	}
	out := 0
	for i := uint32(0); i < n; i++ {
		c := peByte(imageBase, uintptr(s.vaddr)+uintptr(i))
		if c == 0 {
			break
		}
		if c < 0x20 || c == 0x7F {
			c = '.'
		}
		lineBuf[out] = uint16(c)
		out++
	}
	if truncated {
		lineBuf[out] = '.'
		lineBuf[out+1] = '.'
		lineBuf[out+2] = '.'
		out += 3
	}
	lineBuf[out] = 0
	dbg(co, "      content: \"")
	dbgUTF16(co, &lineBuf[0])
	dbg(co, "\"\r\n")
}

// reportUKI prints the presence/absence of each UKI payload section and
// dumps the body of `.cmdline`, `.osrel`, `.uname` when present (those
// three are short ASCII text in any real UKI). Run after walkSections
// has populated the secXxx package-level vars.
func reportUKI(co *efiSimpleTextOutput, imageBase uintptr) {
	dbg(co, "  UKI sections:\r\n")
	reportUKISection(co, ".linux  ", secLinux)
	reportUKISection(co, ".initrd ", secInitrd)
	reportUKISection(co, ".cmdline", secCmdline)
	dumpASCIISection(co, imageBase, secCmdline)
	reportUKISection(co, ".osrel  ", secOsrel)
	dumpASCIISection(co, imageBase, secOsrel)
	reportUKISection(co, ".uname  ", secUname)
	dumpASCIISection(co, imageBase, secUname)

	// Coarse health signal the boot test can grep for.
	found := uint32(0)
	for _, s := range [...]ukiSection{secLinux, secInitrd, secCmdline, secOsrel, secUname} {
		if s.found {
			found++
		}
	}
	dbg(co, "  UKI payload sections found: ")
	dbgDec(co, found)
	dbg(co, "/5\r\n")
}

// ----- LoadFile2 callback (phase 3b, Go-side implementation) -----

// goLoadFile2 implements EFI_LOAD_FILE2_PROTOCOL.LoadFile for our
// embedded `.initrd`. The asm shim in thunk-{amd64,arm64}.S tail-calls
// here — TinyGo's `//go:export` emits the function with the platform
// ABI (MS x64 / AAPCS64), which is what the firmware uses.
//
// The signature mirrors the UEFI spec:
//
//	EFI_STATUS LoadFile(this, devPath, bootPolicy, *bufSize, buf);
//
// On entry:
//   - self/devPath/bootPolicy are ignored — Linux matches us by GUID.
//   - bufSize is an IN/OUT pointer to UINTN.
//   - buf is the firmware-supplied destination buffer.
//
// Contract:
//   - if *bufSize < initrdSize, write initrdSize back and return
//     EFI_BUFFER_TOO_SMALL (0x8000000000000005). The kernel uses this
//     as the size discovery path.
//   - else copy initrdSize bytes from initrdDataPtr to buf, write the
//     copied size back, return EFI_SUCCESS.
//
//go:export goLoadFile2
//go:nosplit
func goLoadFile2(self, devPath, bootPolicy uintptr, bufSizePtr *uint64, buf uintptr) uint64 {
	const efiBufferTooSmall = 0x8000000000000005

	if *bufSizePtr < initrdSize {
		*bufSizePtr = initrdSize
		return efiBufferTooSmall
	}
	for i := uint64(0); i < initrdSize; i++ {
		*(*uint8)(unsafe.Pointer(buf + uintptr(i))) =
			*(*uint8)(unsafe.Pointer(initrdDataPtr + uintptr(i)))
	}
	*bufSizePtr = initrdSize
	return 0
}

// ----- entry point -----

//go:export _start
func _start(imageHandle uintptr, st *efiSystemTable) efiStatus {
	co := st.conOut

	dbg(co, "Hello, UEFI from TinyGo!\r\n")
	dbg(co, "Phase 2 - self-PE inspection\r\n")

	// Locate our own image in memory via the loaded-image protocol.
	// HandleProtocol(handle, &guid, &out) is the 3-arg shorthand for
	// OpenProtocol(handle, &guid, &out, agent, controller, BY_HANDLE);
	// agent + controller are ignored when called this way.
	bs := st.bootServices
	status := efiCall3(bs.handleProtocol,
		imageHandle,
		uintptr(unsafe.Pointer(&loadedImageGUID)),
		uintptr(unsafe.Pointer(&loadedImageHolder)))
	if status != efiSuccess {
		writeASCII(co, "HandleProtocol(LoadedImage) failed: ")
		writeHex64(co, status)
		writeASCII(co, "\r\nPHASE2-FAIL\r\n")
		for {
		}
	}

	lip := (*efiLoadedImageProtocol)(unsafe.Pointer(loadedImageHolder))
	dbg(co, "  imageBase = ")
	dbgHex64(co, uint64(lip.imageBase))
	dbg(co, "\r\n  imageSize = ")
	dbgHex64(co, lip.imageSize)
	dbg(co, "\r\n")

	walkSections(co, lip.imageBase)

	// Sentinel the test grep can lock onto.
	dbg(co, "PHASE2-DONE\r\n")

	// ----- Phase 3a — UKI section discovery -----
	//
	// walkSections has captured any `.linux` / `.initrd` / `.cmdline` /
	// `.osrel` / `.uname` section into the secXxx package-level vars.
	// reportUKI prints the presence table and dumps the body of the
	// short ASCII sections.
	dbg(co, "Phase 3a - UKI section discovery\r\n")
	reportUKI(co, lip.imageBase)
	dbg(co, "PHASE3A-DONE\r\n")

	// ----- Phase 3b — expose .initrd via EFI_LOAD_FILE2_PROTOCOL -----
	//
	// Linux ≥ 5.7 finds an embedded initrd by walking every UEFI handle
	// whose device path is a MEDIA_VENDOR node carrying the
	// LINUX_EFI_INITRD_MEDIA_GUID. We build exactly such a device path,
	// publish a LoadFile2 protocol whose entry point is implemented in
	// asm (see loadFile2 in thunk-{amd64,arm64}.S), and install both on a
	// fresh handle. The asm callback reads `initrdDataPtr` / `initrdSize`
	// — package-level vars we populate just below — to serve the bytes.
	if secInitrd.found && secInitrd.vsize > 0 {
		dbg(co, "Phase 3b - initrd LoadFile2\r\n")

		// MEDIA_DEVICE_PATH (4) / MEDIA_VENDOR (3), length 20 (4-byte
		// header + 16-byte GUID).
		vendorMediaInitrdPath[0] = 0x04
		vendorMediaInitrdPath[1] = 0x03
		vendorMediaInitrdPath[2] = 20
		vendorMediaInitrdPath[3] = 0
		for i := 0; i < 16; i++ {
			vendorMediaInitrdPath[4+i] = linuxInitrdGUID[i]
		}
		// End-of-hardware-device-path node: Type=0x7F, SubType=0xFF, Length=4.
		vendorMediaInitrdPath[20] = 0x7F
		vendorMediaInitrdPath[21] = 0xFF
		vendorMediaInitrdPath[22] = 4
		vendorMediaInitrdPath[23] = 0

		initrdDataPtr = lip.imageBase + uintptr(secInitrd.vaddr)
		initrdSize = uint64(secInitrd.vsize)
		ourLoadFile2.loadFile = loadFile2Ptr()

		// Diagnostic: log the resolved callback address — sanity check
		// that the PC-relative load returned a sensible runtime address
		// inside our image.
		dbg(co, "  loadFile2 fn   = ")
		dbgHex64(co, uint64(ourLoadFile2.loadFile))
		dbg(co, "\r\n  imageBase     = ")
		dbgHex64(co, uint64(lip.imageBase))
		dbg(co, "\r\n  initrdDataPtr = ")
		dbgHex64(co, uint64(initrdDataPtr))
		dbg(co, "\r\n")

		// First InstallProtocolInterface call creates a handle (because
		// initrdHandle starts at 0 / NULL) and binds the DevicePath
		// protocol to it. Second call adds LoadFile2 on the same handle.
		const efiNativeInterface = 0
		status = efiCall4(bs.installProtocolInterface,
			uintptr(unsafe.Pointer(&initrdHandle)),
			uintptr(unsafe.Pointer(&devicePathGUID)),
			efiNativeInterface,
			uintptr(unsafe.Pointer(&vendorMediaInitrdPath[0])))
		if status != efiSuccess {
			writeASCII(co, "InstallProtocolInterface(DevicePath) failed: ")
			writeHex64(co, status)
			writeASCII(co, "\r\nPHASE3B-FAIL\r\n")
			for {
			}
		}
		status = efiCall4(bs.installProtocolInterface,
			uintptr(unsafe.Pointer(&initrdHandle)),
			uintptr(unsafe.Pointer(&loadFile2GUID)),
			efiNativeInterface,
			uintptr(unsafe.Pointer(&ourLoadFile2)))
		if status != efiSuccess {
			writeASCII(co, "InstallProtocolInterface(LoadFile2) failed: ")
			writeHex64(co, status)
			writeASCII(co, "\r\nPHASE3B-FAIL\r\n")
			for {
			}
		}
		dbg(co, "  initrd handle = ")
		dbgHex64(co, uint64(initrdHandle))
		dbg(co, "\r\n  initrd size = ")
		dbgHex64(co, initrdSize)
		dbg(co, "\r\nPHASE3B-DONE\r\n")
	} else {
		dbg(co, "PHASE3B-SKIPPED (no .initrd section)\r\n")
	}

	// ----- Phase 3 (chain-load) -----
	//
	// If the section walk found a `.linux` payload, treat it as an
	// embedded EFI image and chain-load it via BootServices.LoadImage +
	// StartImage. On aarch64 the Linux kernel vmlinuz already IS a
	// PE32+ EFI application (the EFISTUB), so this is the complete
	// handoff — the kernel takes over from here, does its own
	// ExitBootServices, and we never come back. On x86_64 the same
	// path works for any EFI app appended as `.linux`; chain-loading a
	// raw bzImage instead would need the boot-protocol handover entry
	// point, which is left for a later iteration.
	if dotLinuxVA == 0 {
		writeASCII(co, "no .linux section, phase 3 skipped\r\n")
		for {
		}
	}

	dbg(co, "Phase 3 - kernel handoff\r\n")
	dbg(co, "  .linux va=")
	dbgHex64(co, uint64(dotLinuxVA))
	dbg(co, "  size=")
	dbgHex64(co, uint64(dotLinuxSize))
	dbg(co, "\r\n")

	status = efiCall6(bs.loadImage,
		0,                                    // BootPolicy = FALSE: the SourceBuffer holds the image
		imageHandle,                          // ParentImageHandle
		0,                                    // DevicePath = NULL
		lip.imageBase+uintptr(dotLinuxVA),    // SourceBuffer
		uintptr(dotLinuxSize),                // SourceSize
		uintptr(unsafe.Pointer(&childImageHandle)),
	)
	if status != efiSuccess {
		writeASCII(co, "LoadImage failed: ")
		writeHex64(co, status)
		writeASCII(co, "\r\nPHASE3-FAIL\r\n")
		for {
		}
	}
	dbg(co, "  child image handle = ")
	dbgHex64(co, uint64(childImageHandle))
	dbg(co, "\r\n")

	// ----- Phase 3c — propagate .cmdline to the child kernel -----
	//
	// Linux's EFI stub reads its command line from its own loaded-image
	// protocol — image->load_options (UTF-16LE) + image->load_options_size
	// (BYTES, sized for that UTF-16 buffer, NUL terminator optional). The
	// firmware does NOT carry a parent stub's .cmdline section across
	// LoadImage; we have to fetch the child's loaded-image protocol and
	// patch its slots ourselves.
	//
	// We widen the ASCII section in-place (ASCII is a subset of UTF-16LE),
	// trim a trailing newline if `pec` appended one, and NUL-terminate.
	// Skipping silently when no .cmdline section is present matches the
	// pre-fix behaviour for unloaded leaves.
	if secCmdline.found && secCmdline.vsize > 0 {
		dbg(co, "Phase 3c - propagate .cmdline to child LoadOptions\r\n")

		raw := secCmdline.vsize
		maxChars := uint32(len(cmdlineUTF16) - 1) // reserve NUL slot
		if raw > maxChars {
			raw = maxChars
		}
		for raw > 0 {
			c := peByte(lip.imageBase, uintptr(secCmdline.vaddr)+uintptr(raw-1))
			if c != 0 && c != '\n' && c != '\r' {
				break
			}
			raw--
		}
		for i := uint32(0); i < raw; i++ {
			cmdlineUTF16[i] = uint16(peByte(lip.imageBase, uintptr(secCmdline.vaddr)+uintptr(i)))
		}
		cmdlineUTF16[raw] = 0

		status = efiCall3(bs.handleProtocol,
			childImageHandle,
			uintptr(unsafe.Pointer(&loadedImageGUID)),
			uintptr(unsafe.Pointer(&childLIPHolder)))
		if status != efiSuccess {
			// Soft-fail: the kernel will boot with an empty cmdline,
			// which is usually still useful for diagnostics. Log and
			// continue rather than spin forever.
			writeASCII(co, "  child HandleProtocol(LoadedImage) failed: ")
			writeHex64(co, status)
			writeASCII(co, " — booting without cmdline\r\n")
		} else {
			childLip := (*efiLoadedImageProtocol)(unsafe.Pointer(childLIPHolder))
			childLip.loadOptions = uintptr(unsafe.Pointer(&cmdlineUTF16[0]))
			// Include the trailing NUL in the byte count; Linux tolerates
			// either form but systemd-stub conventions favour "with NUL".
			childLip.loadOptionsSize = (raw + 1) * 2
			dbg(co, "  child cmdline bytes = ")
			dbgHex64(co, uint64(childLip.loadOptionsSize))
			dbg(co, "\r\n")
		}
		dbg(co, "PHASE3C-DONE\r\n")
	}

	dbg(co, "StartImage...\r\n")

	// StartImage transfers control. On the happy path it does not return.
	// On failure (a kernel that exits without ExitBootServices, or any
	// other error) we land back here and surface the status.
	status = efiCall3(bs.startImage, childImageHandle, 0, 0)
	writeASCII(co, "StartImage returned: ")
	writeHex64(co, status)
	writeASCII(co, "\r\nPHASE3-RETURNED\r\n")
	for {
	}
}

// main exists only because TinyGo requires a `func main` to consider this
// file a Go program. The firmware never enters here — `_start` (above) is
// the PE entry point, set via `lld-link /entry:_start`.
func main() {}
