package cursor

import (
	"errors"
	"testing"
	"time"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// parkOne opens a session, lets the server call a client tool and parks the session.
func parkOne(t *testing.T, up *fakeUpstream, reg *Registry, callID string) (*Session, *fakeStream) {
	t.Helper()
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("a")}})
	fs.recv()
	fs.send(mcpArgsMsg(1, callID, "a"), false)
	fs.send(listedMsg(1), false)
	for {
		if _, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			break
		}
	}
	if err := reg.Park(s); err != nil {
		t.Fatal(err)
	}
	return s, fs
}

func TestParkedRunExpiresAfterTTL(t *testing.T) {
	up := newFakeUpstream(t)
	clk := newFakeClock()
	reg := NewRegistry(RegistryConfig{TTL: 10 * time.Minute, Clock: clk})
	t.Cleanup(reg.Close)
	s, fs := parkOne(t, up, reg, "c1")

	clk.Advance(10*time.Minute - time.Second)
	if reg.Len() != 1 {
		t.Fatal("run released before its TTL")
	}
	select {
	case <-fs.ctxDone:
		t.Fatal("stream closed before the TTL")
	default:
	}

	clk.Advance(time.Second)
	select {
	case <-fs.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("expired run left its stream open")
	}
	select {
	case <-s.Done():
	case <-time.After(waitTimeout):
		t.Fatal("expired session still running")
	}
	if reg.Len() != 0 {
		t.Error("expired run still registered")
	}
	if _, ok := reg.Take([]string{"c1"}); ok {
		t.Error("Take returned an expired run")
	}
	if err := nextErr(t, s); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v", err)
	}
}

func TestRegistryEvictsTheOldestWhenFull(t *testing.T) {
	up := newFakeUpstream(t)
	reg := NewRegistry(RegistryConfig{MaxParked: 2, Clock: newFakeClock()})
	t.Cleanup(reg.Close)
	s1, fs1 := parkOne(t, up, reg, "c1")
	_, fs2 := parkOne(t, up, reg, "c2")
	_, fs3 := parkOne(t, up, reg, "c3")

	select {
	case <-fs1.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("oldest run not evicted")
	}
	<-s1.Done()
	for _, fs := range []*fakeStream{fs2, fs3} {
		select {
		case <-fs.ctxDone:
			t.Fatal("a newer run was evicted")
		default:
		}
	}
	if reg.Len() != 2 {
		t.Errorf("len = %d, want 2", reg.Len())
	}
	if _, ok := reg.Take([]string{"c1"}); ok {
		t.Error("evicted run still takeable")
	}
}

func TestTakeNeedsEveryPendingCallAnswered(t *testing.T) {
	up := newFakeUpstream(t)
	reg := NewRegistry(RegistryConfig{Clock: newFakeClock()})
	t.Cleanup(reg.Close)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("a")}})
	fs.recv()
	fs.send(mcpArgsMsg(1, "c1", "a"), false)
	fs.send(mcpArgsMsg(2, "c2", "a"), false)
	fs.send(listedMsg(2), false)
	for {
		if _, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			break
		}
	}
	if err := reg.Park(s); err != nil {
		t.Fatal(err)
	}

	if _, ok := reg.Take([]string{"c1"}); ok {
		t.Fatal("a request answering one of two calls resumed the run")
	}
	if _, ok := reg.Take([]string{"other"}); ok {
		t.Fatal("unrelated ids resumed the run")
	}
	if reg.Len() != 1 {
		t.Fatal("a refused Take removed the run")
	}
	got, ok := reg.Take([]string{"c2", "c1", "extra"})
	if !ok || got != s {
		t.Fatal("a request answering every call did not resume the run")
	}
	if reg.Len() != 0 {
		t.Error("taken run still registered")
	}
}

func TestDropClosesTheSupersededRun(t *testing.T) {
	up := newFakeUpstream(t)
	reg := NewRegistry(RegistryConfig{Clock: newFakeClock()})
	t.Cleanup(reg.Close)
	_, fs := parkOne(t, up, reg, "c1")
	reg.Drop([]string{"c1"})
	select {
	case <-fs.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("dropped run still open")
	}
	if reg.Len() != 0 {
		t.Error("dropped run still registered")
	}
}

func TestRegistryForgetsRunsThatEndOnTheirOwn(t *testing.T) {
	up := newFakeUpstream(t)
	reg := NewRegistry(RegistryConfig{Clock: newFakeClock()})
	t.Cleanup(reg.Close)
	_, fs := parkOne(t, up, reg, "c1")
	fs.end(`{"error":{"code":"unavailable","message":"gone"}}`)
	eventually(t, "the registry to forget the run", func() bool { return reg.Len() == 0 })
}

func TestParkNeedsAPendingCallAndRegistryCloseClosesRuns(t *testing.T) {
	up := newFakeUpstream(t)
	reg := NewRegistry(RegistryConfig{Clock: newFakeClock()})
	idle, _ := startSession(t, up, SystemClock{}, RunParams{})
	if err := reg.Park(idle); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("err = %v", err)
	}

	_, fs := parkOne(t, up, reg, "c1")
	reg.Close()
	select {
	case <-fs.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("Close left a parked run open")
	}
}
