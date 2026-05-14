# go-coff/stub

[![CI](https://github.com/go-coff/stub/actions/workflows/ci.yml/badge.svg)](https://github.com/go-coff/stub/actions/workflows/ci.yml)

A UEFI Unified Kernel Image stub written in **TinyGo** + a thin asm
shim. The goal is to remove `systemd` as a build-time dependency for
anyone assembling a UKI: pair this stub with
[`go-coff/pe`](https://github.com/go-coff/pe) /
[`go-coff/pec`](https://github.com/go-coff/pec) and the whole pipeline
runs without `binutils` and without `systemd-stub`.

> **Status:** phase 1 — boots under OVMF (x86_64 and aarch64), prints
> a banner via `SimpleTextOutput`, then spins. Self-PE inspection and
> the kernel handoff land in phase 2 and 3.

## Why TinyGo and not Go

The standard Go runtime is fused to having an OS underneath it
(`mmap`/`VirtualAlloc`, signals, scheduler, GC stop-the-world). UEFI is
a "no OS" environment, so plain Go would refuse to start. TinyGo is
LLVM-based, lets us pick `gc: leaking` and `scheduler: none`, takes a
custom target JSON, and produces ~4 KB binaries. The cost is that we
write a subset of Go that doesn't allocate, doesn't use `reflect`, and
doesn't spawn goroutines. For a stub whose useful work is a few hundred
lines of PE walking and protocol calls, that's an acceptable trade.

## Building

```sh
# Per-arch primitives (substitute x64 or aa64).
task build-x64       # main-x64.o (TinyGo) + thunk-x64.o (clang) → BOOTX64.EFI
task qemu-x64        # boot under OVMF; Ctrl-A x to quit
task qemu-test-x64   # boot, grep the serial log for the banner, fail if absent

task build-aa64      # same pipeline for aarch64 → BOOTAA64.EFI
task qemu-aa64
task qemu-test-aa64

# Aggregates that fan out over both architectures.
task build           # build-x64 + build-aa64
task qemu-test       # qemu-test-x64 + qemu-test-aa64
task clean
```

Dependencies on the host:

- **TinyGo** ≥ 0.41 (`brew tap tinygo-org/tools && brew install tinygo`,
  or the official tarballs on Linux)
- **lld** (the standalone `lld-link` — TinyGo bundles lld as a library,
  not as a binary)
- **QEMU** with OVMF for both architectures (`brew install qemu`;
  on Debian / Ubuntu `qemu-system-x86 qemu-system-arm ovmf
  qemu-efi-aarch64`). On Apple Silicon hosts, `qemu-system-aarch64`
  uses HVF and runs near-native; `qemu-system-x86_64` falls back to
  TCG and is noticeably slower.
- **clang** (already in Xcode CLT on macOS; `clang` package on Linux)

## The six traps we hit (and how to dodge them)

This stub looks small but every line of the toolchain configuration was
paid for in a debugging session. Documented here so the next person
doesn't pay for them again.

### 1. LLVM target triple

`x86_64-unknown-uefi` *is* a real LLVM triple, but TinyGo's vendored
`compiler-rt-builtins/int_lib.h` doesn't recognise it (it switches on
`__ELF__` / `__MINGW32__` / `_WIN32` / `__APPLE__`, none of which Clang
defines for UEFI). The fix is to use **`x86_64-pc-windows-gnu`** — same
PE/COFF output, `__MINGW32__` gets defined, compiler-rt is happy. We
override the EFI subsystem at link time.

### 2. lld is bundled in TinyGo as a library

`tinygo build` doesn't expose `lld-link`. We do the link ourselves with
an external `lld-link` binary (Homebrew `brew install lld`, Debian
`apt-get install lld`).

### 3. Dead Windows runtime symbols

TinyGo with `goos=windows` emits a `mainCRTStartup` entry that calls
`VirtualAlloc`, `exit`, `SystemFunction036`, `abort`, `putchar` — all
of which are undefined in a freestanding link. They are **dead code**:
`/entry:_start` makes them unreachable. We pass `/force:unresolved` so
the linker downgrades the errors to warnings. Anything that actually
calls them at runtime will of course crash; this is a load-bearing
"we don't allocate, we don't panic" discipline.

### 4. `func()` in Go is a fat pointer

A Go function field is **16 bytes** (code pointer + context pointer)
rather than the 8-byte C function pointer the UEFI specification lays
out. Declaring `outputString func(...)` shifts every subsequent field
of `EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL` by 8 bytes, and the firmware looks
nothing like what we expect. **Always declare EFI method slots as
`uintptr`** and route the actual indirect call through a thunk.

### 5. Calling-convention shuffle

TinyGo's calls into external functions on `goos=windows` follow the
platform ABI — MS x64 on amd64 (args in RCX, RDX, R8, R9) and AAPCS64
on aarch64 (args in X0..X7). But the firmware-side function pointer
needs the args in a different position from what TinyGo gave us,
because we pass the fn pointer as the **first** Go arg (so it lands
in the first register that should otherwise hold the first EFI arg).
**One asm thunk per arity per arch** ([`thunk-x64.S`](thunk-x64.S),
[`thunk-aa64.S`](thunk-aa64.S)) handles the shuffle: slide the fn
pointer out of RCX/X0 into a scratch register and slide every other
arg one slot left, then `call`/`blr`.

On aarch64 this is appreciably simpler than on x86_64: a single
calling convention is shared by Linux and Windows ABIs (no MS-vs-SysV
split), no shadow space, and the first six args we ever need (fn + 5
EFI args) all fit in registers without stack juggling.

### 6. Image base above 4 GB defeats the small code model

`lld-link` defaults to `ImageBase = 0x140000000` for PE32+ on amd64.
LLVM's default code model is "small": absolute addresses are encoded
as 32-bit immediates (`mov $abs, %r8d` zero-extends to 64 bits).
0x140003000 doesn't fit in 32 bits, and a `.reloc` entry can't make it
fit either. **Pass `/base:0x10000`** to keep every absolute address
under 4 GB and the encoding becomes valid.

## Layout

```text
stub/
├── main.go                EFI structs (uintptr method slots) + _start + banner
├── thunk-x64.S            MS x64 thunks: efiCall1..5 (RCX/RDX/R8/R9 shuffle)
├── thunk-aa64.S           AAPCS64 thunks: efiCall1..5 (X0..X5 shuffle)
├── targets/
│   ├── uefi-x64.json      TinyGo target: x86_64-pc-windows-gnu + freestanding
│   └── uefi-aa64.json     TinyGo target: aarch64-pc-windows-gnu + freestanding
├── Taskfile.yaml          per-arch: thunk → compile → link → esp → qemu / qemu-test
├── go.mod
├── renovate.json
├── LICENSE                BSD 3-Clause, "The go-coff Authors"
└── .github/workflows/
    └── ci.yml             matrix [x64, aa64] on ubuntu-latest
```

## Roadmap

- **Phase 1** ✅ — print banner via `ConOut->OutputString`, spin.
  Works on both x86_64 and aarch64.
- **Phase 2** — locate our own PE image at runtime via
  `EFI_LOADED_IMAGE_PROTOCOL`, parse the embedded section table, look
  up `.linux` / `.initrd` / `.cmdline`.
- **Phase 3** — install `EFI_LOAD_FILE2_PROTOCOL` on a vendor-media
  device path so the kernel picks up the initrd, jump into the kernel
  via the EFI handover protocol.
- **riscv64** — duplicate the recipe one more time with the
  `riscv64-pc-windows-gnu` triple and a thunk in the RISC-V ABI
  (`a0..a7` arg registers, `ra` link register).

## License

[BSD 3-Clause](LICENSE).
