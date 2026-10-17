//go:build linux && cgo && gztransporttest

package gztransport

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

func TestNativeTransportRoundTrip(t *testing.T) {
	partition := fmt.Sprintf("nervus_go_roundtrip_%d_%d", os.Getpid(), time.Now().UnixNano())
	peer, err := newTransportTestPeer(partition)
	if err != nil {
		t.Fatalf("newTransportTestPeer: %v", err)
	}
	defer peer.close()

	config := DefaultConfig()
	config.Partition = partition
	config.PollInterval = 10 * time.Millisecond
	client, err := Open(config)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.WaitForService(ctx, "/go/echo", "gz.msgs.StringMsg", "gz.msgs.StringMsg"); err != nil {
		t.Fatalf("WaitForService: %v", err)
	}
	response, err := client.Request(ctx, ServiceRequest{
		Service:      "/go/echo",
		RequestType:  "gz.msgs.StringMsg",
		ResponseType: "gz.msgs.StringMsg",
		Data:         encodeTestStringMessage("hello"),
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if value, ok := decodeTestStringMessage(response); !ok || value != "echo:hello" {
		t.Fatalf("echo response = %q, %v", value, ok)
	}

	if err := client.WaitForTopic(ctx, "/go/event", "gz.msgs.StringMsg"); err != nil {
		t.Fatalf("WaitForTopic: %v", err)
	}
	subscription, err := client.Subscribe(
		ctx,
		"/go/event",
		"gz.msgs.StringMsg",
		SubscriptionOptions{CoalesceLatest: true},
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer client.Unsubscribe(context.Background(), subscription)
	if !waitForTestCondition(ctx, peer.eventHasConnections) {
		t.Fatal("event publisher did not connect")
	}
	if !peer.publishEvent("sensor-value") {
		t.Fatal("test peer failed to publish event")
	}
	event, err := client.NextEvent(ctx)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if value, ok := decodeTestStringMessage(event.Data); !ok || value != "sensor-value" {
		t.Fatalf("event payload = %q, %v", value, ok)
	}
	if event.SubscriptionID != subscription || event.Topic != "/go/event" || event.MessageType != "gz.msgs.StringMsg" {
		t.Fatalf("unexpected event metadata: %+v", event)
	}

	command := make([]byte, 9)
	command[0] = 0x11
	binary.LittleEndian.PutUint64(command[1:], math.Float64bits(0.7))
	if err := client.Publish(
		ctx,
		Message{Topic: "/go/command", Type: "gz.msgs.Double", Data: command},
		PublishOptions{RequireConnection: true, ConnectionTimeout: 2 * time.Second},
	); err != nil {
		t.Fatalf("Publish command: %v", err)
	}
	if !waitForTestCondition(ctx, func() bool {
		value, ok := peer.command()
		return ok && math.Abs(value-0.7) < 0.000001
	}) {
		t.Fatal("test peer did not receive Double command")
	}
}

func encodeTestStringMessage(value string) []byte {
	if len(value) >= 128 {
		panic("test string is too large")
	}
	result := make([]byte, len(value)+2)
	result[0] = 0x12
	result[1] = byte(len(value))
	copy(result[2:], value)
	return result
}

func decodeTestStringMessage(data []byte) (string, bool) {
	if len(data) < 2 || data[0] != 0x12 || int(data[1]) != len(data)-2 {
		return "", false
	}
	return string(data[2:]), true
}

func waitForTestCondition(ctx context.Context, condition func() bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
