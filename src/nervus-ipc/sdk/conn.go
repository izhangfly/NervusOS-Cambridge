// This file is the connection layer shared by Client and ServiceHost: dialling, reading and writing Envelopes, closing.
//
// Above it are the two roles (consumer-side Client / provider-side ServiceHost), below it wire.go's framing.
// It's pulled out because the two roles' connection semantics are exactly the same (handshake, read loop, write serialisation, close-once);
// they only differ in how an Envelope is dispatched once read.
package sdk

import (
	"bufio"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// DefaultSockPath is the fixed path of nervud's control-plane UDS (production image).
//
// It's a separate socket from the admin channel /run/nervus/nervud-admin.sock: that one only accepts
// root (the ops tool nervusctl), this one only accepts App-range UIDs. Don't mix them up.
const DefaultSockPath = "/run/nervus/nervud.sock"

// The protocol version this SDK implements.
//
// Aligned with nervud's internal/ipc constants: only this one major is implemented; minors only ever increase,
// and at the handshake the server takes the intersection with the range the client declares.
//
// # Why the lower bound is also 2, rather than keeping 1
//
// v2 is **not** backward compatible with v1; the break is the removal of two implicit defaults: an empty selector in ResolveEndpoint and
// AcquireControl no longer implicitly means {nervus.resource.motion.base,
// main}. A caller written for v1 that leaves the selector empty to request the base's lease gets
// "can't resolve" on v2 instead of the base.
//
// Declaring min=1 would let such connections **complete the handshake**, then give a silent change of meaning
// on some specific call, far more dangerous than failing to connect. So the lower bound rises with the upper bound, and a mismatch
// gets an explicit FAILED_PRECONDITION at the handshake.
const (
	protocolMajorMin = 2
	protocolMajorMax = 2
	protocolMinorMax = 0
)

// conn is an established (but not necessarily handshaken) control-plane connection.
type conn struct {
	c net.Conn

	// Write side: bufio merges the length prefix and body into one system call (UDS has no Nagle, so splitting it into
	// two wouldn't cause latency problems, just one wasted syscall). writeMu serialises concurrent writes:
	// writeFrame takes no lock itself, and two goroutines writing concurrently would interleave Frame bytes.
	w       *bufio.Writer
	writeMu sync.Mutex

	// readBuf is reused per connection, so the read path allocates nothing in the steady state. Its capacity is fixed at the hard limit, so
	// readFrameBody never fails because buf is too small.
	readBuf []byte

	// The handshake negotiation result, read-only after the handshake.
	negMajor, negMinor uint32
	packageID          string
	componentID        string
	limits             *ipcv1.ConnectionLimits

	closeOnce sync.Once
	closed    chan struct{}
}

// dial connects to nervud's control-plane UDS.
//
// Only unix domain sockets are supported: control-plane identity comes from the kernel through SO_PEERCRED, which has no
// TCP equivalent. Making the network a configurable parameter would only tempt people to run over TCP, which would break the whole
// identity model.
func dial(sockPath string, timeout time.Duration) (*conn, error) {
	if sockPath == "" {
		sockPath = DefaultSockPath
	}
	c, err := net.DialTimeout("unix", sockPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("sdk: dial %s: %w", sockPath, err)
	}
	return &conn{
		c:       c,
		w:       bufio.NewWriterSize(c, 4096),
		readBuf: make([]byte, MaxFrameBytes),
		closed:  make(chan struct{}),
	}, nil
}

// writeEnvelope serialises and writes out an Envelope, holding the write lock throughout.
//
// After the handshake protocol_major/minor are **not** filled in: the protocol version is a connection property, decided by connection
// state after the handshake, and receivers ignore these two fields in later Envelopes (envelope.proto says so explicitly).
// proto3 doesn't send zero values, so these two empty fields take not a single byte.
func (co *conn) writeEnvelope(env *ipcv1.Envelope) error {
	b, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("sdk: marshal envelope: %w", err)
	}

	co.writeMu.Lock()
	defer co.writeMu.Unlock()

	select {
	case <-co.closed:
		return ErrClosed
	default:
	}

	if err := writeFrame(co.w, b); err != nil {
		// After a failed write the stream can't be recovered (the length may have gone out without the body), so the connection is discarded
		co.close()
		return err
	}
	if err := co.w.Flush(); err != nil {
		co.close()
		return fmt.Errorf("sdk: flush: %w", err)
	}
	return nil
}

// readEnvelope reads one complete Envelope.
//
// **Calling constraint**: only one reader goroutine may call this function: readBuf is reused, and bytes fields in the returned
// Envelope may reference it (protobuf decoding copies, but don't rely on that),
// and concurrent reads would make two goroutines fight over the same byte stream's boundaries.
func (co *conn) readEnvelope() (*ipcv1.Envelope, error) {
	n, err := readFrameHeader(co.c)
	if err != nil {
		return nil, err
	}
	body, err := readFrameBody(co.c, co.readBuf, n)
	if err != nil {
		return nil, err
	}
	env := &ipcv1.Envelope{}
	if err := proto.Unmarshal(body, env); err != nil {
		return nil, fmt.Errorf("%w: malformed envelope: %v", ErrProtocol, err)
	}
	if env.GetBody() == nil {
		// An unset body (including unknown bodies from an unknown newer version) is always treated as a protocol violation.
		// An empty Envelope has no legitimate use, and tolerating it would leave a gap for "sending a pile of empty frames to burn the budget".
		return nil, fmt.Errorf("%w: envelope has no body", ErrProtocol)
	}
	return env, nil
}

// setReadDeadline sets the deadline for the next read. The zero value clears it.
func (co *conn) setReadDeadline(t time.Time) error { return co.c.SetReadDeadline(t) }

// keepAlive sends a Ping every half of the idle timeout nervud announced at the handshake, until the connection closes.
//
// nervud drops a connection that sends nothing for IdleTimeout (60 s by default), and only Ping/Pong keeps a quiet one
// open. Answering the server's Pings isn't enough: nervud doesn't send any. Without this, a provider nobody calls (piperd
// with no app open, camerad with no viewer) lost its connection every minute and was restarted by systemd, and a call that
// arrived during the restart failed. The Kotlin SDK has done the same since the start (NervusClient.runPingLoop).
func (co *conn) keepAlive(ids *requestIDGen) {
	idle := time.Duration(co.limits.GetIdleTimeoutMs()) * time.Millisecond
	if idle <= 0 {
		return
	}
	t := time.NewTicker(idle / 2)
	defer t.Stop()
	for {
		select {
		case <-co.closed:
			return
		case <-t.C:
		}
		nonce, err := ids.nextID()
		if err != nil {
			return
		}
		if err := co.writeEnvelope(&ipcv1.Envelope{Body: &ipcv1.Envelope_Ping{Ping: &ipcv1.Ping{Nonce: nonce}}}); err != nil {
			return
		}
	}
}

// close closes the underlying connection idempotently.
func (co *conn) close() {
	co.closeOnce.Do(func() {
		close(co.closed)
		_ = co.c.Close()
	})
}

// Limits returns this connection's budgets sent by nervud at the handshake.
//
// These values are **guides for SDK self-discipline, not server promises**: the server doesn't relax enforcement because it announced them.
// Going over still gets refused or disconnected.
func (co *conn) Limits() *ipcv1.ConnectionLimits { return co.limits }
