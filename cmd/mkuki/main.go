// mkuki builds a test UKI by taking a freshly-linked BOOTxxx.EFI and
// using github.com/go-coff/pe to append a fixed set of UKI payload
// sections. It is meant for the phase-3a integration test: produces a
// stub variant where reportUKI can actually find .cmdline / .osrel /
// .uname content and prove the section discovery works end-to-end.
//
// Usage:
//
//	mkuki out.efi in.efi
//
// The injected sections are hard-coded. If you need a real UKI (with
// a kernel + initrd), reach for the companion `pec` CLI instead.
package main

import (
	"fmt"
	"os"

	"github.com/go-coff/pe"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mkuki <out.efi> <in.efi>")
		os.Exit(2)
	}
	out := os.Args[1]
	in := os.Args[2]

	stub, err := os.ReadFile(in)
	must(err)

	// Fixed test payloads. The body of each is short enough to fit
	// inside the stub's lineBuf at phase-3a print time, which keeps the
	// "content: …" lines fully visible without the `…` truncation.
	sections := []pe.Section{
		{
			Name:            ".cmdline",
			Data:            []byte("console=ttyS0 quiet rw"),
			Characteristics: pe.DefaultCharacteristics,
		},
		{
			Name:            ".osrel",
			Data:            []byte("ID=go-coff-test\nVERSION_ID=0\nPRETTY_NAME=\"go-coff test UKI\"\n"),
			Characteristics: pe.DefaultCharacteristics,
		},
		{
			Name:            ".uname",
			Data:            []byte("6.10.0-test"),
			Characteristics: pe.DefaultCharacteristics,
		},
	}

	res, err := pe.Append(stub, sections)
	must(err)
	must(os.WriteFile(out, res, 0o644))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkuki:", err)
		os.Exit(1)
	}
}
