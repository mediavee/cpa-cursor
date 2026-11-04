package cursor

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// Defaults for RegistryConfig (D6).
const (
	DefaultParkTTL   = 10 * time.Minute
	DefaultMaxParked = 64
)

// ErrNothingPending is returned when parking a session that waits on no tool call.
var ErrNothingPending = errors.New("cursor: session has no pending tool call to park on")

// RegistryConfig bounds the parked runs.
type RegistryConfig struct {
	// TTL is how long a parked run waits for its results. DefaultParkTTL when zero.
	TTL time.Duration
	// MaxParked bounds the parked runs; parking one more closes the oldest.
	// DefaultMaxParked when zero.
	MaxParked int
	// Clock defaults to the system clock.
	Clock Clock
}

// Registry holds the sessions parked on client tool calls, indexed by
// tool_call_id, until the results come back, the TTL runs out or the
// registry is full.
type Registry struct {
	ttl   time.Duration
	max   int
	clock Clock

	mu     sync.Mutex
	byCall map[string]*parked
	order  []*parked // oldest first
	closed bool
}

type parked struct {
	s     *Session
	ids   []string
	timer Timer
	// gone is closed when the parking leaves the registry, which ends its watcher.
	gone chan struct{}
}

func NewRegistry(cfg RegistryConfig) *Registry {
	r := &Registry{ttl: cfg.TTL, max: cfg.MaxParked, clock: cfg.Clock, byCall: map[string]*parked{}}
	if r.ttl <= 0 {
		r.ttl = DefaultParkTTL
	}
	if r.max <= 0 {
		r.max = DefaultMaxParked
	}
	if r.clock == nil {
		r.clock = SystemClock{}
	}
	return r
}

// Park takes over a session that emitted a ToolCallsEvent. The session is
// detached from the context of the request that opened it and keeps
// heartbeating. It is closed when the TTL expires, when it is evicted by a
// newer one, or when the stream ends on its own.
func (r *Registry) Park(s *Session) error {
	ids := s.PendingToolCalls()
	if len(ids) == 0 {
		return ErrNothingPending
	}
	p := &parked{s: s, ids: ids, gone: make(chan struct{})}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		s.Close()
		return ErrClosed
	}
	var evicted []*parked
	for len(r.order) >= r.max {
		evicted = append(evicted, r.order[0])
		r.removeLocked(r.order[0])
	}
	s.Unbind()
	p.timer = r.clock.AfterFunc(r.ttl, func() { r.expire(p) })
	r.order = append(r.order, p)
	for _, id := range ids {
		r.byCall[id] = p
	}
	r.mu.Unlock()

	for _, e := range evicted {
		e.s.Close()
	}
	go func() {
		select {
		case <-s.Done():
			r.expire(p)
		case <-p.gone:
		}
	}()
	return nil
}

// Take removes and returns the parked session that waits on tool calls whose
// ids are all in toolCallIDs, the ids of the results a request brings. The
// caller then Binds the session to its own context and submits the results.
// It returns false when nothing is parked for these ids or when the request
// does not answer every pending call; in that case the session stays parked.
func (r *Registry) Take(toolCallIDs []string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range toolCallIDs {
		p, ok := r.byCall[id]
		if !ok {
			continue
		}
		pending := p.s.PendingToolCalls()
		if len(pending) == 0 || !subset(pending, toolCallIDs) {
			return nil, false
		}
		r.removeLocked(p)
		return p.s, true
	}
	return nil, false
}

// Drop closes the parked sessions that wait on any of the given tool calls.
// A request that replays a conversation instead of resuming the run uses it
// to release the run it supersedes.
func (r *Registry) Drop(toolCallIDs []string) {
	r.mu.Lock()
	var dropped []*parked
	for _, id := range toolCallIDs {
		if p, ok := r.byCall[id]; ok {
			dropped = append(dropped, p)
			r.removeLocked(p)
		}
	}
	r.mu.Unlock()
	for _, p := range dropped {
		p.s.Close()
	}
}

func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.order)
}

// Close closes every parked session and refuses new ones.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	all := slices.Clone(r.order)
	for _, p := range all {
		r.removeLocked(p)
	}
	r.mu.Unlock()
	for _, p := range all {
		p.s.Close()
	}
}

func (r *Registry) expire(p *parked) {
	r.mu.Lock()
	_, registered := r.contains(p)
	if registered {
		r.removeLocked(p)
	}
	r.mu.Unlock()
	if registered {
		p.s.Close()
	}
}

func (r *Registry) contains(p *parked) (int, bool) {
	i := slices.Index(r.order, p)
	return i, i >= 0
}

func (r *Registry) removeLocked(p *parked) {
	i, ok := r.contains(p)
	if !ok {
		return
	}
	r.order = slices.Delete(r.order, i, i+1)
	close(p.gone)
	for _, id := range p.ids {
		if r.byCall[id] == p {
			delete(r.byCall, id)
		}
	}
	p.timer.Stop()
}

func subset(sub, of []string) bool {
	for _, v := range sub {
		if !slices.Contains(of, v) {
			return false
		}
	}
	return true
}
