// Package bus is a tiny in-process pub/sub for realtime (SSE) fan-out. The
// instance-per-app model (single process, single writer) is exactly why this
// needs no external broker: appends publish here, live subscribers drain.
//
// Correctness rests on globalSeq, not on the bus: a slow subscriber is dropped
// (its channel closes) rather than blocking the writer, and reconnects with its
// last globalSeq to catch up from the log. No event is ever lost — only re-read.
package bus

import (
	"sync"

	"github.com/dotlabshq/foldbase/internal/store"
)

const bufferSize = 256

type subscriber struct {
	ch   chan store.StoredEvent
	over chan struct{} // closed once when the buffer overflows (drop → reconnect)
	once sync.Once
}

// Bus fans out appended events to live subscribers, keyed by tenant.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[int]*subscriber
	next int
}

func New() *Bus {
	return &Bus{subs: map[string]map[int]*subscriber{}}
}

// Subscribe returns an id, the event channel, and an "overflow" channel that
// closes if the subscriber falls too far behind (signal to end the stream).
func (b *Bus) Subscribe(tenant string) (int, <-chan store.StoredEvent, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &subscriber{ch: make(chan store.StoredEvent, bufferSize), over: make(chan struct{})}
	id := b.next
	b.next++
	if b.subs[tenant] == nil {
		b.subs[tenant] = map[int]*subscriber{}
	}
	b.subs[tenant][id] = s
	return id, s.ch, s.over
}

func (b *Bus) Unsubscribe(tenant string, id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.subs[tenant]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(b.subs, tenant)
		}
	}
}

// Publish delivers an event to every subscriber of its tenant, non-blocking. A
// full subscriber is signalled to overflow (it will reconnect and catch up).
func (b *Bus) Publish(tenant string, ev store.StoredEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs[tenant] {
		select {
		case s.ch <- ev:
		default:
			s.once.Do(func() { close(s.over) })
		}
	}
}
