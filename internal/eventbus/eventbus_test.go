package eventbus

import (
	"testing"
	"time"
)

func TestPublishReachesSubscribers(t *testing.T) {
	b := New()
	id1, ch1 := b.Subscribe()
	_, ch2 := b.Subscribe()
	defer b.Unsubscribe(id1)

	if got := b.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount = %d, want 2", got)
	}

	want := Event{SessionID: "s1", Source: SourceNative, Kind: KindResult, Text: "PONG"}
	b.Publish(want)

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.SessionID != "s1" || got.Kind != KindResult || got.Text != "PONG" {
				t.Errorf("subscriber %d got %+v", i, got)
			}
			if got.TS.IsZero() {
				t.Errorf("subscriber %d: TS should be auto-stamped", i)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: timed out waiting for event", i)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := New()
	id, ch := b.Subscribe()
	b.Unsubscribe(id)

	// Channel is closed after Unsubscribe.
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after Unsubscribe")
	}
	// Publishing after unsubscribe must not panic.
	b.Publish(Event{SessionID: "s2", Kind: KindOutput})
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount = %d, want 0", got)
	}
}

func TestPublishDoesNotBlockOnFullSubscriber(t *testing.T) {
	b := New()
	b.buf = 1 // tiny buffer to force a drop
	id, _ := b.Subscribe()
	defer b.Unsubscribe(id)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Event{SessionID: "s3", Kind: KindOutput})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
}
