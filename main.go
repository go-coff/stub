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
// indirect calls go through asm thunks in thunk-{x64,aa64}.S that translate
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
	// Truncated past here; firmware fills the rest in but we never read
	// it, so leaving the struct short is harmless.
}

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

// ----- Thunks (see thunk-{x64,aa64}.S) -----

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

	writeASCII(co, "  sections: ")
	writeDec(co, uint32(numSections))
	writeASCII(co, "\r\n")

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

		writeASCII(co, "    ")
		writeUTF16(co, &nameBuf[0])
		writeASCII(co, "  va=")
		writeHex64(co, uint64(vaddr))
		writeASCII(co, "  size=")
		writeHex64(co, uint64(vsize))
		writeASCII(co, "\r\n")
	}
}

// ----- entry point -----

//go:export _start
func _start(imageHandle uintptr, st *efiSystemTable) efiStatus {
	co := st.conOut

	writeASCII(co, "Hello, UEFI from TinyGo!\r\n")
	writeASCII(co, "Phase 2 - self-PE inspection\r\n")

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
	writeASCII(co, "  imageBase = ")
	writeHex64(co, uint64(lip.imageBase))
	writeASCII(co, "\r\n  imageSize = ")
	writeHex64(co, lip.imageSize)
	writeASCII(co, "\r\n")

	walkSections(co, lip.imageBase)

	// Sentinel the test grep can lock onto.
	writeASCII(co, "PHASE2-DONE\r\n")

	for {
	}
}

// main exists only because TinyGo requires a `func main` to consider this
// file a Go program. The firmware never enters here — `_start` (above) is
// the PE entry point, set via `lld-link /entry:_start`.
func main() {}
