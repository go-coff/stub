// AAPCS64 thunks that adapt TinyGo's external-call register layout to
// the calling convention every EFI service uses on aarch64.
//
// On aarch64, Windows and Linux/SysV agree on the procedure-call
// standard (AAPCS64): the first eight integer args land in X0..X7,
// the return value comes back in X0, and the link register is X30.
// So the shuffle is straightforward — slide the fn pointer out of
// X0 into a scratch register (X9, which is intra-procedure call
// corruptible) and shift every subsequent arg one slot left, then
// BLR into the firmware.
//
// Because all five EFI arities we ship below fit inside the
// register-passed range (6 total args max — fn + 5 EFI args), no
// stack juggling is needed. The 16-byte alignment rule on aarch64 is
// satisfied trivially since we only push the (X29, X30) pair.
//
// Signatures (Go side):
//
//   //go:linkname efiCallN efiCallN
//   func efiCallN(fn, a, b, ...) uint64
//
// Register flow (efiCall2 shown):
//
//   In:  X0=fn  X1=a  X2=b
//   Out: X0=a   X1=b  then BLR X9 (with X9=fn)

    .text

    // efiCall1(fn, a) -> uint64
    .globl efiCall1
efiCall1:
    stp x29, x30, [sp, #-16]!
    mov x29, sp
    mov x9, x0
    mov x0, x1
    blr x9
    ldp x29, x30, [sp], #16
    ret

    // efiCall2(fn, a, b) -> uint64
    .globl efiCall2
efiCall2:
    stp x29, x30, [sp, #-16]!
    mov x29, sp
    mov x9, x0
    mov x0, x1
    mov x1, x2
    blr x9
    ldp x29, x30, [sp], #16
    ret

    // efiCall3(fn, a, b, c) -> uint64
    .globl efiCall3
efiCall3:
    stp x29, x30, [sp, #-16]!
    mov x29, sp
    mov x9, x0
    mov x0, x1
    mov x1, x2
    mov x2, x3
    blr x9
    ldp x29, x30, [sp], #16
    ret

    // efiCall4(fn, a, b, c, d) -> uint64
    .globl efiCall4
efiCall4:
    stp x29, x30, [sp, #-16]!
    mov x29, sp
    mov x9, x0
    mov x0, x1
    mov x1, x2
    mov x2, x3
    mov x3, x4
    blr x9
    ldp x29, x30, [sp], #16
    ret

    // efiCall5(fn, a, b, c, d, e) -> uint64
    .globl efiCall5
efiCall5:
    stp x29, x30, [sp, #-16]!
    mov x29, sp
    mov x9, x0
    mov x0, x1
    mov x1, x2
    mov x2, x3
    mov x3, x4
    mov x4, x5
    blr x9
    ldp x29, x30, [sp], #16
    ret

    // efiCall6(fn, a, b, c, d, e, f) -> uint64
    .globl efiCall6
efiCall6:
    stp x29, x30, [sp, #-16]!
    mov x29, sp
    mov x9, x0
    mov x0, x1
    mov x1, x2
    mov x2, x3
    mov x3, x4
    mov x4, x5
    mov x5, x6
    blr x9
    ldp x29, x30, [sp], #16
    ret

    // ----- LoadFile2 callback (phase 3b) -----
    //
    // EFI_STATUS loadFile2(self, devPath, bootPolicy, *bufSize, buf)
    //
    // AAPCS64 (firmware → us):
    //   X0 = self        (ignored)
    //   X1 = devPath     (ignored)
    //   X2 = bootPolicy  (ignored)
    //   X3 = *bufSize    (IN/OUT pointer to UINTN)
    //   X4 = buf
    //
    // Same protocol as the x86_64 sibling above — see comments there.
    .globl loadFile2
loadFile2:
    adrp    x5, initrdSize
    add     x5, x5, :lo12:initrdSize
    ldr     x5, [x5]                 // x5 = our initrd size
    ldr     x6, [x3]                 // x6 = *bufSize
    cmp     x6, x5
    b.lo    .Lsmall_aa64             // unsigned <, buffer too small

    // Copy x5 bytes from initrdDataPtr → buf (x4). Scalar byte loop:
    // small initrds boot just as quickly, and we avoid any alignment
    // surprise on the firmware-owned buffer.
    adrp    x7, initrdDataPtr
    add     x7, x7, :lo12:initrdDataPtr
    ldr     x7, [x7]                 // x7 = initrdDataPtr
    mov     x8, #0                   // loop index
.Lcopy_aa64:
    cmp     x8, x5
    b.ge    .Ldone_aa64
    ldrb    w9, [x7, x8]
    strb    w9, [x4, x8]
    add     x8, x8, #1
    b       .Lcopy_aa64
.Ldone_aa64:
    str     x5, [x3]                 // *bufSize = initrdSize
    mov     x0, #0                   // EFI_SUCCESS
    ret

.Lsmall_aa64:
    str     x5, [x3]                 // *bufSize = needed size
    // EFI_BUFFER_TOO_SMALL = 0x8000000000000005. Build it in two steps:
    // low 16 bits = 5, then OR the MSB.
    mov     x0, #5
    movk    x0, #0x8000, lsl #48
    ret

    // BSS-resident globals shared with the Go side. Same scheme as the
    // x86_64 sibling above; Go binds via //go:linkname.
    .bss
    .globl initrdSize
    .balign 8
initrdSize:
    .quad 0

    .globl initrdDataPtr
    .balign 8
initrdDataPtr:
    .quad 0

    .data
    .globl loadFile2Entry
    .balign 8
loadFile2Entry:
    .quad loadFile2
