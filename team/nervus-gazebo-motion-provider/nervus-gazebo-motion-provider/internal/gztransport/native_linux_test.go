//go:build linux && cgo

package gztransport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeClientLifecycle(t *testing.T) {
	config := DefaultConfig()
	config.Partition = fmt.Sprintf("nervus_go_adapter_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	config.PollInterval = 10 * time.Millisecond
	config.PublisherConnectTimeout = 40 * time.Millisecond
	config.DefaultRequestTimeout = 50 * time.Millisecond

	client, err := Open(config)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer client.Close()

	versions := client.Versions()
	if versions.ABI != 1 || !strings.HasPrefix(versions.Transport, "15.") || !strings.HasPrefix(versions.Messages, "12.") {
		t.Fatalf("unexpected versions: %+v", versions)
	}
	staticVersions, err := NativeVersions()
	if err != nil {
		t.Fatalf("NativeVersions: %v", err)
	}
	if staticVersions != versions {
		t.Fatalf("static versions %+v differ from client versions %+v", staticVersions, versions)
	}

	ctx := context.Background()
	if topics, err := client.ListTopics(ctx); err != nil || topics == nil {
		t.Fatalf("ListTopics = %v, %v", topics, err)
	}
	if services, err := client.ListServices(ctx); err != nil || services == nil {
		t.Fatalf("ListServices = %v, %v", services, err)
	}
	if _, err := client.TopicInfo(ctx, "/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TopicInfo missing error = %v", err)
	}
	if _, err := client.ServiceInfo(ctx, "/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ServiceInfo missing error = %v", err)
	}

	waitContext, cancelWait := context.WithTimeout(ctx, 35*time.Millisecond)
	err = client.WaitForTopic(waitContext, "/missing", "gz.msgs.StringMsg")
	cancelWait()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForTopic error = %v, want context deadline", err)
	}

	id, err := client.Subscribe(ctx, "/events", "gz.msgs.StringMsg", SubscriptionOptions{CoalesceLatest: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if id == 0 {
		t.Fatal("Subscribe returned ID zero")
	}
	if _, err := client.Subscribe(ctx, "/events", "gz.msgs.StringMsg", SubscriptionOptions{}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Subscribe error = %v", err)
	}

	eventContext, cancelEvent := context.WithTimeout(ctx, 35*time.Millisecond)
	_, err = client.NextEvent(eventContext)
	cancelEvent()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NextEvent error = %v, want context deadline", err)
	}

	if err := client.Unsubscribe(ctx, id); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if err := client.Unsubscribe(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Unsubscribe error = %v", err)
	}

	err = client.Publish(
		ctx,
		Message{Topic: "/no_subscriber", Type: "gz.msgs.Double"},
		PublishOptions{RequireConnection: true},
	)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Publish without subscriber error = %v", err)
	}

	_, err = client.Request(ctx, ServiceRequest{
		Service:      "/missing",
		RequestType:  "gz.msgs.StringMsg",
		ResponseType: "gz.msgs.StringMsg",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Request missing service error = %v", err)
	}

	waitResult := make(chan error, 1)
	go func() {
		_, waitErr := client.NextEvent(context.Background())
		waitResult <- waitErr
	}()
	time.Sleep(20 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-waitResult:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked NextEvent after close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release blocked NextEvent")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := client.ListTopics(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTopics after close error = %v", err)
	}
}

func TestNativeClientGoValidation(t *testing.T) {
	client, err := Open(Config{Partition: fmt.Sprintf("nervus_go_validation_%d", os.Getpid())})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer client.Close()

	if _, err := client.TopicInfo(nil, "/topic"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context error = %v", err)
	}
	if err := client.Publish(context.Background(), Message{}, PublishOptions{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty publish error = %v", err)
	}
	if _, err := client.Request(context.Background(), ServiceRequest{Timeout: -time.Second}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid request error = %v", err)
	}
	if err := client.Unsubscribe(context.Background(), 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero subscription error = %v", err)
	}
}
