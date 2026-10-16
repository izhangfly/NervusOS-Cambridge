package sdk

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// fakeNervud is a test double for nervud's control plane: it answers on a UDS following nervud's state machine.
//
// It's **not** a second implementation of nervud, only covering the round trips the SDK needs to assert: the handshake,
// ResolveEndpoint, Request→Response, Dispatch→DispatchResult. Its behaviour deliberately follows the key disciplines of
// internal/ipc (the first frame must be Hello, the server echoes identity,
// unimplemented bodies close the connection), so passing here means something when connecting to the real nervud.
//
// The counterpart of the Python SDK's former _fakenervud.py.
type fakeNervud struct {
	t  *testing.T
	ln net.Listener

	mu sync.Mutex
	// onEnvelope is called on receiving an Envelope and returns the Envelope to send back (nil = no reply).
	// Tests use it to inject all kinds of replies and faults.
	onEnvelope func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope

	// received records every Envelope the server received, for assertions.
	received []*ipcv1.Envelope

	wg     sync.WaitGroup
	closed bool
}

// startFakeNervud starts a UDS in a temporary directory.
//
// A real socket rather than net.Pipe: dial hard-codes network="unix", and that hard-coding is itself
// deliberate (control plane identity relies on SO_PEERCRED, which has no TCP equivalent). The tests follow the real path.
func startFakeNervud(t *testing.T) *fakeNervud {
	t.Helper()
	// UDS paths have a length limit (sun_path is about 108 bytes), and in some environments t.TempDir()
	// puts the long test name into the path; use a short prefix under the system temporary directory.
	dir, err := os.MkdirTemp("", "nipc-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeNervud{t: t, ln: ln}
	f.wg.Add(1)
	go f.acceptLoop()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeNervud) sockPath() string { return f.ln.Addr().String() }

func (f *fakeNervud) setHandler(fn func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope) {
	f.mu.Lock()
	f.onEnvelope = fn
	f.mu.Unlock()
}

func (f *fakeNervud) stop() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	_ = f.ln.Close()
	f.wg.Wait()
}

func (f *fakeNervud) acceptLoop() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return // listener closed
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.serveConn(c)
		}()
	}
}

func (f *fakeNervud) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	buf := make([]byte, MaxFrameBytes)
	for {
		n, err := readFrameHeader(c)
		if err != nil {
			return
		}
		body, err := readFrameBody(c, buf, n)
		if err != nil {
			return
		}
		env := &ipcv1.Envelope{}
		if err := proto.Unmarshal(body, env); err != nil {
			return
		}

		f.mu.Lock()
		f.received = append(f.received, env)
		fn := f.onEnvelope
		f.mu.Unlock()

		if fn == nil {
			continue
		}
		for _, out := range fn(c, env) {
			if out == nil {
				continue
			}
			b, err := proto.Marshal(out)
			if err != nil {
				return
			}
			if err := writeFrame(c, b); err != nil {
				return
			}
		}
	}
}

// recvCount returns the number of Envelopes the server has received so far.
func (f *fakeNervud) recvCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

// ---- Common reply builders ---------------------------------------------------------

const (
	testPackageID   = "com.example.test"
	testComponentID = "main"
)

// helloAckOK is the standard successful handshake reply.
func helloAckOK() *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_HelloAck{HelloAck: &ipcv1.HelloAck{
		Outcome: &ipcv1.HelloAck_Success{Success: &ipcv1.HelloAckSuccess{
			ProtocolMajor: protocolMajorMax,
			ProtocolMinor: protocolMinorMax,
			PackageId:     testPackageID,
			ComponentId:   testComponentID,
			Limits: &ipcv1.ConnectionLimits{
				MaxFrameBytes:             MaxFrameBytes,
				MaxInflightRequests:       64,
				DefaultTimeoutMs:          1000,
				MaxTimeoutMs:              30000,
				MaxSubscriptions:          8,
				IdleTimeoutMs:             60000,
				MaxInflightPayloadBytes:   1 << 20,
				MaxOutboundQueueBytes:     512 << 10,
				DefaultMethodPayloadBytes: 16 << 10,
			},
		}},
	}}}
}

// helloAckFailure is a refused handshake reply.
func helloAckFailure(code ipcv1.StatusCode) *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_HelloAck{HelloAck: &ipcv1.HelloAck{
		Outcome: &ipcv1.HelloAck_Failure{Failure: &ipcv1.Failure{Code: code}},
	}}}
}

// autoHandshake returns a handler that only handles the handshake, passing the rest to next.
func autoHandshake(next func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope) func(net.Conn, *ipcv1.Envelope) []*ipcv1.Envelope {
	return func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetHello() != nil {
			return []*ipcv1.Envelope{helloAckOK()}
		}
		if next == nil {
			return nil
		}
		return next(c, env)
	}
}
