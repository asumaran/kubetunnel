package logging

import (
	"sync"
)

// RingBuffer is a bounded, goroutine-safe ring of Entries with a pub/sub API
// for live tailers. Old entries are dropped when capacity is reached.
type RingBuffer struct {
	mu       sync.RWMutex
	entries  []Entry
	capacity int
	start    int // index of oldest entry
	size     int
	nextSub  int
	subs     map[int]chan Entry
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &RingBuffer{
		entries:  make([]Entry, capacity),
		capacity: capacity,
		subs:     make(map[int]chan Entry),
	}
}

func (r *RingBuffer) Append(e Entry) {
	r.mu.Lock()
	idx := (r.start + r.size) % r.capacity
	if r.size < r.capacity {
		r.entries[idx] = e
		r.size++
	} else {
		r.entries[r.start] = e
		r.start = (r.start + 1) % r.capacity
	}
	// Copy subscriber channels so we can release the lock before sending.
	chans := make([]chan Entry, 0, len(r.subs))
	for _, ch := range r.subs {
		chans = append(chans, ch)
	}
	r.mu.Unlock()

	for _, ch := range chans {
		// Non-blocking send with overflow semantics: slow consumers drop.
		select {
		case ch <- e:
		default:
		}
	}
}

// Snapshot returns a copy of up to `limit` most recent entries.
// If limit <= 0 or exceeds size, returns all entries oldest-first.
func (r *RingBuffer) Snapshot(limit int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.size == 0 {
		return nil
	}
	n := r.size
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Entry, n)
	// Copy the last n entries in chronological order.
	begin := (r.start + r.size - n + r.capacity) % r.capacity
	for i := 0; i < n; i++ {
		out[i] = r.entries[(begin+i)%r.capacity]
	}
	return out
}

// Subscribe returns a channel that receives new entries plus an unsubscribe
// function. The buffer is 64 — slow consumers will drop events, not block
// producers.
func (r *RingBuffer) Subscribe() (<-chan Entry, func()) {
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	ch := make(chan Entry, 64)
	r.subs[id] = ch
	r.mu.Unlock()
	unsub := func() {
		r.mu.Lock()
		if ch, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(ch)
		}
		r.mu.Unlock()
	}
	return ch, unsub
}

// Size returns the current number of entries.
func (r *RingBuffer) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// SubscriberCount returns the number of active subscribers.
func (r *RingBuffer) SubscriberCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}
