//go:build linux && cgo && gztransporttest

package gztransport

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct gz_test_peer gz_test_peer_t;

int gz_test_peer_create(const char *partition, gz_test_peer_t **peer);
void gz_test_peer_destroy(gz_test_peer_t *peer);
int gz_test_peer_publish_event(gz_test_peer_t *peer, const char *value);
int gz_test_peer_event_has_connections(gz_test_peer_t *peer);
int gz_test_peer_command(gz_test_peer_t *peer, double *value);
*/
import "C"

import "unsafe"

type transportTestPeer struct {
	handle *C.gz_test_peer_t
}

func newTransportTestPeer(partition string) (*transportTestPeer, error) {
	nativePartition := C.CString(partition)
	defer C.free(unsafe.Pointer(nativePartition))
	var handle *C.gz_test_peer_t
	if C.gz_test_peer_create(nativePartition, &handle) == 0 || handle == nil {
		return nil, &Error{Status: StatusInternal, Operation: "create test peer", Detail: "native test peer initialization failed"}
	}
	return &transportTestPeer{handle: handle}, nil
}

func (p *transportTestPeer) close() {
	if p == nil || p.handle == nil {
		return
	}
	C.gz_test_peer_destroy(p.handle)
	p.handle = nil
}

func (p *transportTestPeer) publishEvent(value string) bool {
	if p == nil || p.handle == nil {
		return false
	}
	nativeValue := C.CString(value)
	defer C.free(unsafe.Pointer(nativeValue))
	return C.gz_test_peer_publish_event(p.handle, nativeValue) != 0
}

func (p *transportTestPeer) eventHasConnections() bool {
	return p != nil && p.handle != nil && C.gz_test_peer_event_has_connections(p.handle) != 0
}

func (p *transportTestPeer) command() (float64, bool) {
	if p == nil || p.handle == nil {
		return 0, false
	}
	var value C.double
	if C.gz_test_peer_command(p.handle, &value) == 0 {
		return 0, false
	}
	return float64(value), true
}
