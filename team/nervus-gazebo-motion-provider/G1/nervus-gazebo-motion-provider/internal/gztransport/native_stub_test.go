//go:build !linux || !cgo

package gztransport

import (
	"errors"
	"testing"
)

func TestNativeStubFailsClosed(t *testing.T) {
	client, err := Open(Config{})
	if client != nil {
		t.Fatal("stub Open returned a client")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stub Open error = %v", err)
	}
	if _, err := NativeVersions(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stub NativeVersions error = %v", err)
	}
}
