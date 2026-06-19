// LoongArch64 LP64D thunks that adapt TinyGo's external-call register
// layout to the UEFI calling convention on loongarch64.
//
// LP64D ABI (LoongArch ELF psABI v2):
//   - integer args:  a0..a7 ($r4..$r11)
//   - return value:  a0 ($r4)
//   - link register: ra ($r1)
//   - stack pointer: sp ($r3)
//   - stack alignment: 16 bytes
//   - caller-saved scratch: t0..t8 ($r12..$r20)
//
// TinyGo emits its Go-side cgo trampoline using the same ABI, so the
// shuffle is identical to the RV64 / AAPCS64 shape: slide the function
// pointer out of a0 into t0 (a caller-saved scratch), shift every
// subsequent arg one slot left, then `jirl ra, t0, 0` (jump-and-link
// register, the LoongArch analog of RV64's `jalr`). The link register
// holds the return address; we stash it on the stack across the call.
//
// Six arities (efiCall1..efiCall6) cover every EFI service we invoke.
// All of them fit in the register-passed range (max 7 total args =
// fn + 6 EFI args), so no stack juggling is required.

    .text

    .globl efiCall1
efiCall1:
    addi.d   $sp, $sp, -16
    st.d     $ra, $sp, 0
    move     $t0, $a0          # save fn
    move     $a0, $a1          # shift args left
    jirl     $ra, $t0, 0
    ld.d     $ra, $sp, 0
    addi.d   $sp, $sp, 16
    ret

    .globl efiCall2
efiCall2:
    addi.d   $sp, $sp, -16
    st.d     $ra, $sp, 0
    move     $t0, $a0
    move     $a0, $a1
    move     $a1, $a2
    jirl     $ra, $t0, 0
    ld.d     $ra, $sp, 0
    addi.d   $sp, $sp, 16
    ret

    .globl efiCall3
efiCall3:
    addi.d   $sp, $sp, -16
    st.d     $ra, $sp, 0
    move     $t0, $a0
    move     $a0, $a1
    move     $a1, $a2
    move     $a2, $a3
    jirl     $ra, $t0, 0
    ld.d     $ra, $sp, 0
    addi.d   $sp, $sp, 16
    ret

    .globl efiCall4
efiCall4:
    addi.d   $sp, $sp, -16
    st.d     $ra, $sp, 0
    move     $t0, $a0
    move     $a0, $a1
    move     $a1, $a2
    move     $a2, $a3
    move     $a3, $a4
    jirl     $ra, $t0, 0
    ld.d     $ra, $sp, 0
    addi.d   $sp, $sp, 16
    ret

    .globl efiCall5
efiCall5:
    addi.d   $sp, $sp, -16
    st.d     $ra, $sp, 0
    move     $t0, $a0
    move     $a0, $a1
    move     $a1, $a2
    move     $a2, $a3
    move     $a3, $a4
    move     $a4, $a5
    jirl     $ra, $t0, 0
    ld.d     $ra, $sp, 0
    addi.d   $sp, $sp, 16
    ret

    .globl efiCall6
efiCall6:
    addi.d   $sp, $sp, -16
    st.d     $ra, $sp, 0
    move     $t0, $a0
    move     $a0, $a1
    move     $a1, $a2
    move     $a2, $a3
    move     $a3, $a4
    move     $a4, $a5
    move     $a5, $a6
    jirl     $ra, $t0, 0
    ld.d     $ra, $sp, 0
    addi.d   $sp, $sp, 16
    ret

    # ----- LoadFile2 callback (phase 3b) -----
    #
    # UEFI calls `loadFile2` with the LP64 calling convention. TinyGo
    # emits Go functions with the same convention on linux/loong64, so
    # the asm shim is just a tail-call into the Go implementation.
    #
    # `b goLoadFile2` is a 28-bit PC-relative branch (±128 MiB). Within
    # any UEFI image that easily covers the .text section, so no need
    # for the pcaddu18i + jirl long-range pair.
    .globl loadFile2
loadFile2:
    b        goLoadFile2

    # loadFile2Ptr returns the runtime address of loadFile2.
    # `la.pcrel $a0, loadFile2` expands to a pcalau12i + addi.d pair —
    # PC-relative, emitting R_LARCH_PCALA_HI20 + R_LARCH_PCALA_LO12
    # relocs that peln's reloc_loongarch64.go handles natively. No
    # .reloc fixup needed, so the address survives any post-link
    # section shuffling done by `pec.Append`.
    .globl loadFile2Ptr
loadFile2Ptr:
    la.pcrel $a0, loadFile2
    ret
