module github.com/go-coff/stub

go 1.25.1

require github.com/diskfs/go-diskfs v1.9.3

require (
	github.com/djherbis/times v1.6.0 // indirect
	github.com/go-coff/pe v0.0.0
	golang.org/x/sys v0.43.0 // indirect
)

replace github.com/go-coff/pe => ../pe
