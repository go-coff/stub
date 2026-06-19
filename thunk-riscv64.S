// RV64 LP64 thunks that adapt TinyGo's external-call register layout to
// the UEFI calling convention on riscv64.
//
// RV64 LP64 ABI:
//   - integer args:  a0..a7 (x10..x17)
//   - return value:  a0
//   - link register: ra (x1)
//   - stack pointer: sp (x2)
//   - stack alignment: 16 bytes
//   - caller-saved scratch: t0..t6 (x5..x7, x28..x31)
//
// TinyGo emits its Go-side cgo trampoline using the same ABI, so the
// shuffle is identical to AAPCS64: slide the function pointer out of a0
// into t0 (a caller-saved scratch), shift every subsequent arg one slot
// left, then jalr t0. The link register holds the return address; we
// stash it on the stack across the call.
//
// Six arities (efiCall1..efiCall6) cover every EFI service we invoke.
// All of them fit in the register-passed range (max 7 total args =
// fn + 6 EFI args), so no stack juggling is required.

    .text

    .globl efiCall1
efiCall1:
    addi sp, sp, -16
    sd   ra, 0(sp)
    mv   t0, a0          // save fn
    mv   a0, a1          // shift args left
    jalr t0
    ld   ra, 0(sp)
    addi sp, sp, 16
    ret

    .globl efiCall2
efiCall2:
    addi sp, sp, -16
    sd   ra, 0(sp)
    mv   t0, a0
    mv   a0, a1
    mv   a1, a2
    jalr t0
    ld   ra, 0(sp)
    addi sp, sp, 16
    ret

    .globl efiCall3
efiCall3:
    addi sp, sp, -16
    sd   ra, 0(sp)
    mv   t0, a0
    mv   a0, a1
    mv   a1, a2
    mv   a2, a3
    jalr t0
    ld   ra, 0(sp)
    addi sp, sp, 16
    ret

    .globl efiCall4
efiCall4:
    addi sp, sp, -16
    sd   ra, 0(sp)
    mv   t0, a0
    mv   a0, a1
    mv   a1, a2
    mv   a2, a3
    mv   a3, a4
    jalr t0
    ld   ra, 0(sp)
    addi sp, sp, 16
    ret

    .globl efiCall5
efiCall5:
    addi sp, sp, -16
    sd   ra, 0(sp)
    mv   t0, a0
    mv   a0, a1
    mv   a1, a2
    mv   a2, a3
    mv   a3, a4
    mv   a4, a5
    jalr t0
    ld   ra, 0(sp)
    addi sp, sp, 16
    ret

    .globl efiCall6
efiCall6:
    addi sp, sp, -16
    sd   ra, 0(sp)
    mv   t0, a0
    mv   a0, a1
    mv   a1, a2
    mv   a2, a3
    mv   a3, a4
    mv   a4, a5
    mv   a5, a6
    jalr t0
    ld   ra, 0(sp)
    addi sp, sp, 16
    ret

    // ----- LoadFile2 callback (phase 3b) -----
    //
    // UEFI calls `loadFile2` with the LP64 calling convention. TinyGo
    // emits Go functions with the same convention on linux/riscv64, so
    // the asm shim is just a tail-call into the Go implementation.
    //
    // We need the asm symbol so the protocol-struct pointer we install
    // is a flat C entry point, not a Go fat function value.
    .globl loadFile2
loadFile2:
    tail goLoadFile2

    // loadFile2Ptr returns the runtime address of loadFile2.
    // `la a0, loadFile2` expands to an auipc + addi pair — PC-relative,
    // no .reloc fixup needed, so it survives any post-link section
    // shuffling done by `pec.Append`.
    .globl loadFile2Ptr
loadFile2Ptr:
    la   a0, loadFile2
    ret
