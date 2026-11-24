package notify

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDispatcherDoesNotBlockCaller(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	d := newDispatcher(func(string, string, any) error {
		startedOnce.Do(func() { close(started) })
		<-release
		return nil
	})

	if !d.enqueue(request{title: "first"}) {
		t.Fatal("first notification was unexpectedly dropped")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("notification worker did not start")
	}

	done := make(chan bool, 1)
	go func() { done <- d.enqueue(request{title: "second"}) }()
	select {
	case accepted := <-done:
		if !accepted {
			t.Fatal("notification should fit in the queue")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("enqueue blocked behind the notification backend")
	}

	if d.enqueue(request{title: "third"}) {
		t.Fatal("notification should be dropped when the queue is full")
	}
	close(release)
}

func TestDispatcherReportsBackendError(t *testing.T) {
	want := errors.New("notification backend failed")
	d := newDispatcher(func(string, string, any) error { return want })
	errCh := make(chan error, 1)

	if !d.enqueue(request{title: "test", onError: func(err error) { errCh <- err }}) {
		t.Fatal("notification was unexpectedly dropped")
	}
	select {
	case got := <-errCh:
		if !errors.Is(got, want) {
			t.Fatalf("callback error = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("error callback was not called")
	}
}
