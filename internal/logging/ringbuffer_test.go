package logging

import (
	"sync"
	"testing"
	"time"
)

func TestRingBufferAppendAndSnapshot(t *testing.T) {
	rb := NewRingBuffer(3)
	if rb.Size() != 0 {
		t.Errorf("expected empty, got size=%d", rb.Size())
	}

	for i := 0; i < 5; i++ {
		rb.Append(Entry{Time: time.Now(), Msg: string(rune('a' + i))})
	}
	if rb.Size() != 3 {
		t.Errorf("expected size 3, got %d", rb.Size())
	}
	snap := rb.Snapshot(0)
	if len(snap) != 3 {
		t.Fatalf("expected 3 snapshot, got %d", len(snap))
	}
	// Should contain the 3 most recent: c, d, e
	if snap[0].Msg != "c" || snap[1].Msg != "d" || snap[2].Msg != "e" {
		t.Errorf("unexpected snapshot order: %v %v %v", snap[0].Msg, snap[1].Msg, snap[2].Msg)
	}
}

func TestRingBufferSnapshotLimit(t *testing.T) {
	rb := NewRingBuffer(10)
	for i := 0; i < 5; i++ {
		rb.Append(Entry{Msg: string(rune('a' + i))})
	}
	snap := rb.Snapshot(2)
	if len(snap) != 2 {
		t.Fatalf("expected 2, got %d", len(snap))
	}
	if snap[0].Msg != "d" || snap[1].Msg != "e" {
		t.Errorf("expected d,e got %v,%v", snap[0].Msg, snap[1].Msg)
	}
}

func TestRingBufferSubscribe(t *testing.T) {
	rb := NewRingBuffer(10)
	ch, unsub := rb.Subscribe()
	defer unsub()

	var wg sync.WaitGroup
	wg.Add(1)
	received := 0
	go func() {
		defer wg.Done()
		for range ch {
			received++
			if received == 3 {
				return
			}
		}
	}()

	for i := 0; i < 3; i++ {
		rb.Append(Entry{Msg: "x"})
	}
	wg.Wait()
	if received != 3 {
		t.Errorf("expected 3 events, got %d", received)
	}
}

func TestRingBufferUnsubscribeClosesChannel(t *testing.T) {
	rb := NewRingBuffer(10)
	ch, unsub := rb.Subscribe()
	if rb.SubscriberCount() != 1 {
		t.Errorf("expected 1 subscriber, got %d", rb.SubscriberCount())
	}
	unsub()
	if rb.SubscriberCount() != 0 {
		t.Errorf("expected 0 after unsub, got %d", rb.SubscriberCount())
	}
	// Reading from a closed channel should not block.
	_, ok := <-ch
	if ok {
		t.Error("expected channel closed")
	}
}

func TestRingBufferSlowConsumerDoesNotBlock(t *testing.T) {
	rb := NewRingBuffer(1000)
	_, unsub := rb.Subscribe()
	defer unsub()
	// Never read — producer must not block beyond buffer.
	for i := 0; i < 200; i++ {
		rb.Append(Entry{Msg: "y"})
	}
}
