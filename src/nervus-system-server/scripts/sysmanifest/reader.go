package main

import (
	"bytes"
	"io"
)

// newReader is a thin wrapper around bytes.NewReader, only so the parsing in main.go reads more plainly.
func newReader(b []byte) io.Reader { return bytes.NewReader(b) }
