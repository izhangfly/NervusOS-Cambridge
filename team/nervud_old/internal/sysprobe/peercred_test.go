//go:build linux

package sysprobe

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dialSelf starts a UDS listener, connects to it, and returns the server side of the connection.
// Connecting to ourselves is the strongest assertion a single process can make: the peer's PID/UID/GID are this process's own,
// so they can be compared exactly, one by one, instead of only asserting that the values are non-zero.
func dialSelf(t *testing.T) *net.UnixConn {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "t.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1) // capacity 1: the goroutine does not leak even if Accept fails
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	a := <-ch
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}
	t.Cleanup(func() { _ = a.conn.Close() })

	server, ok := a.conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("accept returned %T, want *net.UnixConn", a.conn)
	}
	return server
}

func TestPeerCred_SelfConnect(t *testing.T) {
	cred, err := PeerCred(dialSelf(t))
	if err != nil {
		t.Fatalf("PeerCred: %v", err)
	}

	if got, want := int(cred.PID), os.Getpid(); got != want {
		t.Errorf("PID = %d, want %d", got, want)
	}
	if got, want := int(cred.UID), os.Getuid(); got != want {
		t.Errorf("UID = %d, want %d", got, want)
	}
	if got, want := int(cred.GID), os.Getgid(); got != want {
		t.Errorf("GID = %d, want %d", got, want)
	}
}

// TestPeerCred_PreservesDeadlines is a regression lock on the implementation, not on the return value.
//
// If someone changes PeerCred to get the fd with c.File(), File() dups the fd and switches the original connection
// to blocking mode; the netpoller stops managing it and SetReadDeadline silently stops working, so
// the Read below would hang forever (caught by go test's timeout) instead of returning a timeout error on schedule.
//
// The control plane's slowloris protection rests entirely on deadlines (section 10.11), so this property must be
// locked down by a test, not just written in a comment.
func TestPeerCred_PreservesDeadlines(t *testing.T) {
	server := dialSelf(t)

	if _, err := PeerCred(server); err != nil {
		t.Fatalf("PeerCred: %v", err)
	}

	if err := server.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	// The peer is connected but never sends anything: the only thing that can make Read return is the deadline.
	_, err := server.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read err = %v, want os.ErrDeadlineExceeded", err)
	}
}

func TestPeerCred_NilConn(t *testing.T) {
	// Return a clear error rather than panic: a nil connection is the caller's assembly mistake, which the layer above should be able to log and locate, not crash the whole kernel over
	if _, err := PeerCred(nil); err == nil {
		t.Fatal("want error for nil conn")
	}
}
