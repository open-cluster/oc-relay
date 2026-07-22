package session

import (
	"math/rand"
	"time"
)

// backoff produces reconnect delays that grow exponentially from base to a cap. Jitter
// is injected so a fleet that lost the control plane together does not reconnect in
// lockstep and stampede it back into overload.
type backoff struct {
	config  backoffConfig
	current time.Duration
}

type backoffConfig struct {
	base   time.Duration
	max    time.Duration
	jitter func(time.Duration) time.Duration
}

func newBackoff(config backoffConfig) *backoff {
	return &backoff{config: config}
}

// next returns the next delay and advances the exponential step (capped at max). The
// returned value is jittered; the exponential progression tracks the un-jittered ceiling
// so jitter never compounds.
func (b *backoff) next() time.Duration {
	if b.current == 0 {
		b.current = b.config.base
	} else {
		b.current *= 2
		if b.current > b.config.max {
			b.current = b.config.max
		}
	}

	return b.config.jitter(b.current)
}

// reset returns to the base delay after a successful connection.
func (b *backoff) reset() {
	b.current = 0
}

// fullJitter spreads the delay uniformly across [0, step]: a relay may retry immediately
// or wait up to the full capped step, never longer.
func fullJitter(r *rand.Rand) func(time.Duration) time.Duration {
	return func(d time.Duration) time.Duration {
		if d <= 0 {
			return 0
		}
		return time.Duration(r.Int63n(int64(d) + 1))
	}
}
