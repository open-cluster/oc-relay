package session

import (
	"math/rand"
	"testing"
	"time"
)

// Reconnect backoff must grow exponentially, be capped, and be jittered so a fleet of
// relays that all lost the control plane at once do not reconnect in lockstep.
func TestBackoff_GrowsExponentiallyThenCaps(t *testing.T) {
	b := newBackoff(backoffConfig{
		base: 100 * time.Millisecond,
		max:  2 * time.Second,
		// Deterministic: no jitter, so the ceiling of each step is exact.
		jitter: func(d time.Duration) time.Duration { return d },
	})

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		2 * time.Second, // capped
		2 * time.Second, // stays capped
	}
	for i, w := range want {
		got := b.next()
		if got != w {
			t.Fatalf("step %d: got %s, want %s", i, got, w)
		}
	}
}

func TestBackoff_ResetReturnsToBase(t *testing.T) {
	b := newBackoff(backoffConfig{
		base:   100 * time.Millisecond,
		max:    2 * time.Second,
		jitter: func(d time.Duration) time.Duration { return d },
	})
	b.next()
	b.next()
	b.reset()
	if got := b.next(); got != 100*time.Millisecond {
		t.Fatalf("after reset: got %s, want base 100ms", got)
	}
}

func TestBackoff_JitterStaysWithinTheStep(t *testing.T) {
	// Full jitter: the delay is a random value in [0, step]. A relay must never wait
	// longer than the capped step, and must sometimes retry quickly.
	b := newBackoff(backoffConfig{
		base:   time.Second,
		max:    time.Second,
		jitter: fullJitter(rand.New(rand.NewSource(1))),
	})
	for i := 0; i < 100; i++ {
		got := b.next()
		if got < 0 || got > time.Second {
			t.Fatalf("jittered delay %s out of [0, 1s]", got)
		}
	}
}
