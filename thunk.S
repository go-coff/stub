// Microsoft x64 thunks that adapt TinyGo's external-call register layout
// to the calling convention every EFI service uses.
//
// Why we need this: a Go `func()` value is a fat pointer (code+context,
// 16 bytes wide) and Go's call sites don't always emit raw MS x64 args
// for function-value calls. So instead of putting EFI services into
// `func` fields and calling them through Go, we keep the firmware
// function pointers as plain `uintptr` and route every call through one
// of the thunks below. The Go declaration is, e.g.:
//
//   //go:linkname efiCall2 efiCall2
//   func efiCall2(fn, a, b uintptr) uint64
//
// On a TinyGo / goarch=amd64 / goos=windows external call the first
// four integer args land in RCX, RDX, R8, R9 (already MS x64). Each
// thunk just shifts the fn pointer out of RCX into RAX, slides every
// other arg one slot to the right, then calls `*RAX`. The shadow space
// (`sub $0x28`) keeps RSP 16-byte aligned at the inner CALL.
//
// Caller stack layout on entry to a 5-arg variant:
//
//   [rsp+0x00] return address
//   [rsp+0x08..0x27] caller-reserved shadow space (unused by us)
//   [rsp+0x28] 5th caller arg (1st arg that didn't fit in regs)
//   [rsp+0x30] 6th caller arg
//
// MS x64 requires that the stack be 16-byte aligned **before** the
// inner CALL. On entry RSP is 8 mod 16 (the outer CALL pushed 8 bytes
// of return address onto a 16-aligned stack). `sub $0x28` brings RSP
// back to 16-aligned (0x28 ≡ 8 mod 16, 8+8 ≡ 0 mod 16).

    .text

    // efiCall1(fn, a) -> uint64
    .globl efiCall1
efiCall1:
    subq    $0x28, %rsp
    movq    %rcx, %rax
    movq    %rdx, %rcx
    callq   *%rax
    addq    $0x28, %rsp
    retq

    // efiCall2(fn, a, b) -> uint64
    .globl efiCall2
efiCall2:
    subq    $0x28, %rsp
    movq    %rcx, %rax
    movq    %rdx, %rcx
    movq    %r8,  %rdx
    callq   *%rax
    addq    $0x28, %rsp
    retq

    // efiCall3(fn, a, b, c) -> uint64
    .globl efiCall3
efiCall3:
    subq    $0x28, %rsp
    movq    %rcx, %rax
    movq    %rdx, %rcx
    movq    %r8,  %rdx
    movq    %r9,  %r8
    callq   *%rax
    addq    $0x28, %rsp
    retq

    // efiCall4(fn, a, b, c, d) -> uint64
    .globl efiCall4
efiCall4:
    movq    0x28(%rsp), %r10        // d (5th caller arg, on caller's stack)
    subq    $0x28, %rsp
    movq    %rcx, %rax
    movq    %rdx, %rcx
    movq    %r8,  %rdx
    movq    %r9,  %r8
    movq    %r10, %r9
    callq   *%rax
    addq    $0x28, %rsp
    retq

    // efiCall5(fn, a, b, c, d, e) -> uint64
    .globl efiCall5
efiCall5:
    movq    0x28(%rsp), %r10        // d (5th caller arg)
    movq    0x30(%rsp), %r11        // e (6th caller arg)
    movq    %rcx, %rax              // rax = fn
    movq    %rdx, %rcx              // rcx = a
    movq    %r8,  %rdx              // rdx = b
    movq    %r9,  %r8               // r8  = c
    movq    %r10, %r9               // r9  = d
    subq    $0x28, %rsp             // shadow + 8 for stack-passed arg
    movq    %r11, 0x20(%rsp)        // e at [rsp+0x20] (MS x64 5th arg slot)
    callq   *%rax
    addq    $0x28, %rsp
    retq
