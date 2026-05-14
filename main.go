// Command stub is a UEFI Unified Kernel Image stub written in TinyGo.
//
// At this stage (phase 1) it only prints a banner via the firmware
// SimpleTextOutput protocol and spins. Phases 2 and 3 will add self-PE
// inspection (locating embedded .linux / .initrd / .cmdline sections at
// runtime) and the kernel handoff.
package main

import "unsafe"

// EFI_STATUS is UINTN (same width as a pointer). On amd64 it's uint64.
type efiStatus = uint64

const efiSuccess efiStatus = 0

// EFI_TABLE_HEADER (24 bytes), leads every standard EFI table.
type efiTableHeader struct {
	signature  uint64
	revision   uint32
	headerSize uint32
	crc32      uint32
	_reserved  uint32
}

// EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL — every method slot is a uintptr so
// the struct layout matches the firmware-side C layout byte-for-byte.
// A Go `func` field would be a fat pointer (16 bytes) and would shift
// every subsequent field by 8 bytes, breaking ConOut->outputString
// dereferences.
type efiSimpleTextOutput struct {
	reset           uintptr // 0x00
	outputString    uintptr // 0x08  EFI_TEXT_STRING
	testString      uintptr // 0x10
	queryMode       uintptr // 0x18
	setMode         uintptr // 0x20
	setAttribute    uintptr // 0x28
	clearScreen     uintptr // 0x30
	setCursorPos    uintptr // 0x38
	enableCursor    uintptr // 0x40
	mode            uintptr // 0x48
}

// EFI_SYSTEM_TABLE — only the prefix up to ConOut is fleshed out, since
// that's all the phase-1 hello-world needs. The rest is opaque.
type efiSystemTable struct {
	hdr                 efiTableHeader      // 0x00, 24 bytes
	firmwareVendor      *uint16             // 0x18
	firmwareRevision    uint32              // 0x20
	_pad1               uint32              // 0x24 (alignment to 8)
	consoleInHandle     uintptr             // 0x28
	conIn               uintptr             // 0x30
	consoleOutHandle    uintptr             // 0x38
	conOut              *efiSimpleTextOutput // 0x40
}

// Thunks (see thunk.S). One per arity; they shift the fn pointer into
// RAX and slide the actual args into MS x64 register positions.
//
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

// Pre-encoded UTF-16LE banner. Keeping it as a fixed-size array means we
// never need to allocate on the print path — important because TinyGo's
// "leaking" allocator would crash here (no heap is set up).
var banner = [...]uint16{
	'H', 'e', 'l', 'l', 'o', ',', ' ',
	'U', 'E', 'F', 'I', ' ',
	'f', 'r', 'o', 'm', ' ',
	'T', 'i', 'n', 'y', 'G', 'o', '!',
	'\r', '\n', 0,
}

//go:export _start
func _start(imageHandle uintptr, st *efiSystemTable) efiStatus {
	co := st.conOut
	efiCall2(co.outputString,
		uintptr(unsafe.Pointer(co)),
		uintptr(unsafe.Pointer(&banner[0])))

	// Spin so the firmware keeps the banner on screen instead of
	// returning to BdsDxe and re-entering the boot menu.
	for {
	}
}

// main exists only because TinyGo requires a `func main` to consider
// the file a Go program. The firmware never enters here — our `_start`
// (above) is the PE entry point, set via `lld-link /entry:_start`.
func main() {}
