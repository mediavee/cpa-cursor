package cursor

import "time"

// Clock is the time source of sessions and of the parked-run registry. It is
// an interface so that tests can drive heartbeats, stall detection and TTLs
// without sleeping.
type Clock interface {
	Now() time.Time
	// NewTimer behaves like time.NewTimer.
	NewTimer(d time.Duration) Timer
	// AfterFunc behaves like time.AfterFunc: f runs in its own goroutine.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of *time.Timer the package uses.
type Timer interface {
	// C is nil for timers created by AfterFunc.
	C() <-chan time.Time
	Reset(d time.Duration) bool
	Stop() bool
}

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) NewTimer(d time.Duration) Timer { return sysTimer{time.NewTimer(d)} }

func (SystemClock) AfterFunc(d time.Duration, f func()) Timer { return sysTimer{time.AfterFunc(d, f)} }

type sysTimer struct{ t *time.Timer }

func (s sysTimer) C() <-chan time.Time        { return s.t.C }
func (s sysTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }
func (s sysTimer) Stop() bool                 { return s.t.Stop() }
