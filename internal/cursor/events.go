package cursor

import (
	"context"
	"crypto/sha256"
	"sync"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// Event is one thing a run reports to its consumer. The generated messages
// are exposed as the server sent them; the session adds no abstraction.
type Event interface{ isEvent() }

// UpdateEvent carries an interaction_update: text and thinking deltas,
// turn_ended with the token usage, routed_model, tool_requests_listed and the
// rest. Pure heartbeats are consumed by the session and never surface.
type UpdateEvent struct{ Update *agentv1.InteractionUpdate }

// ToolCallsEvent reports the client tool calls of one step of the model. The
// session has not answered them: it stays open, heartbeating, until
// SubmitToolResults has been called for each call.
type ToolCallsEvent struct{ Calls []*agentv1.McpArgs }

// TtftEvent carries the time-to-first-token breakdown, including the wait in
// the slow pool.
type TtftEvent struct{ Breakdown *agentv1.TtftBreakdown }

// CheckpointEvent carries a conversation_checkpoint_update.
type CheckpointEvent struct {
	State *agentv1.ConversationStateStructure
}

func (UpdateEvent) isEvent()     {}
func (ToolCallsEvent) isEvent()  {}
func (TtftEvent) isEvent()       {}
func (CheckpointEvent) isEvent() {}

// eventQueue is an unbounded FIFO with a single consumer. It is unbounded so
// that a slow consumer never stops the session from reading, which would
// look like a stall to the server and to our own watchdog.
type eventQueue struct {
	mu     sync.Mutex
	items  []Event
	done   bool
	err    error
	notify chan struct{}
}

func newEventQueue() *eventQueue { return &eventQueue{notify: make(chan struct{}, 1)} }

func (q *eventQueue) push(ev Event) {
	q.mu.Lock()
	if q.done {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.wake()
}

// finish ends the queue: queued events are still delivered, then pop returns err.
func (q *eventQueue) finish(err error) {
	q.mu.Lock()
	if !q.done {
		q.done, q.err = true, err
	}
	q.mu.Unlock()
	q.wake()
}

func (q *eventQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *eventQueue) pop(ctx context.Context) (Event, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			ev := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]
			q.mu.Unlock()
			return ev, nil
		}
		done, err := q.done, q.err
		q.mu.Unlock()
		if done {
			return nil, err
		}
		select {
		case <-q.notify:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// Blobs is the per-run key-value store the server uses for conversation
// state. The caller pre-fills it with the root prompt blobs.
type Blobs struct {
	mu sync.RWMutex
	m  map[string][]byte
}

func NewBlobs() *Blobs { return &Blobs{m: map[string][]byte{}} }

// Put stores data under its sha256, the id scheme of the CLI, and returns the id.
func (b *Blobs) Put(data []byte) []byte {
	id := sha256.Sum256(data)
	b.Set(id[:], data)
	return id[:]
}

// Set stores data under an arbitrary id.
func (b *Blobs) Set(id, data []byte) {
	b.mu.Lock()
	b.m[string(id)] = data
	b.mu.Unlock()
}

func (b *Blobs) Get(id []byte) ([]byte, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	data, ok := b.m[string(id)]
	return data, ok
}
