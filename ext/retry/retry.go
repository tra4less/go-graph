// Package retry is a go-graph middleware that re-runs the tasks it wraps
// when they fail.
//
// Register it once and use it in a document's wrap: lists:
//
//	graph.Middleware("retry", retry.Retry)
//
//	- detail:
//	    wrap:
//	      - retry:
//	          attempts: 3
//	          backoff: 20ms
//	    do:
//	      - fill:
//	          call: fill_detail
//
// Each attempt runs on a fresh overlay, so a failed attempt leaves no writes
// behind; the outcome is the last attempt's. Retry only tasks that are safe
// to repeat: reads, or writes that are idempotent.
package retry

import (
	"context"
	"errors"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

// defaultAttempts is used when Config.Attempts is zero, as it is for a
// wrap: entry without config or under graph.WrapCalls.
const defaultAttempts = 3

// Config is the config of a retry entry in a wrap: list.
type Config struct {
	// Attempts is the total number of runs, the first included. Zero means 3.
	Attempts int `yaml:"attempts"`
	// Backoff is the wait before the second attempt; it doubles after each
	// failure. Zero retries at once.
	Backoff time.Duration `yaml:"backoff"`
	// MaxBackoff caps the wait. Zero means no cap.
	MaxBackoff time.Duration `yaml:"maxBackoff"`
}

// Validate reports a config graph.Compile must reject: negative attempts or
// waits, or a backoff larger than maxBackoff.
func (c Config) Validate() error {
	switch {
	case c.Attempts < 0:
		return errors.New("attempts must be >= 0 (0 means the default)")
	case c.Backoff < 0, c.MaxBackoff < 0:
		return errors.New("backoff and maxBackoff must be >= 0")
	case c.MaxBackoff > 0 && c.Backoff > c.MaxBackoff:
		return errors.New("backoff must not exceed maxBackoff")
	}
	return nil
}

// Input is the middleware's input: its config, no blackboard keys.
type Input struct {
	Cfg Config `graph:",with"`
}

// Retry retries every failure except panics, and stops as soon as ctx is
// done: a cancelled caller or an expired timeout ends the retries.
func Retry(ctx context.Context, w graph.Wrap[Input]) error {
	return run(ctx, w, notPanic)
}

// When returns a Retry that retries only the failures for which retryable
// reports true. Panics and a done ctx still end the retries.
func When(retryable func(error) bool) func(context.Context, graph.Wrap[Input]) error {
	return func(ctx context.Context, w graph.Wrap[Input]) error {
		return run(ctx, w, func(err error) bool { return notPanic(err) && retryable(err) })
	}
}

// notPanic reports whether err is not, and does not wrap, the failure of a
// task whose user code panicked.
func notPanic(err error) bool {
	for err != nil {
		var te *graph.TaskError
		if !errors.As(err, &te) {
			return true
		}
		if te.Panic != nil {
			return false
		}
		err = te.Err
	}
	return true
}

func run(ctx context.Context, w graph.Wrap[Input], retry func(error) bool) error {
	cfg := w.In.Cfg
	attempts := cfg.Attempts
	if attempts == 0 {
		attempts = defaultAttempts
	}
	wait := cfg.Backoff
	var err error
	for i := range attempts {
		if i > 0 && wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return err
			}
			wait *= 2
			if cfg.MaxBackoff > 0 && wait > cfg.MaxBackoff {
				wait = cfg.MaxBackoff
			}
		}
		if err = w.Next(ctx); err == nil || ctx.Err() != nil || !retry(err) {
			return err
		}
	}
	return err
}
