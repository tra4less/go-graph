package retry_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
	"github.com/tra4less/go-graph/ext/retry"
)

// flaky fails the first n calls, then succeeds.
func flaky(n int, err error) (next func(context.Context) error, calls *int) {
	calls = new(int)
	return func(context.Context) error {
		if *calls++; *calls <= n {
			return err
		}
		return nil
	}, calls
}

func wrap(cfg retry.Config, next func(context.Context) error) graph.Wrap[retry.Input] {
	return graph.Wrap[retry.Input]{In: retry.Input{Cfg: cfg}, Next: next}
}

func TestRetry(t *testing.T) {
	transient := errors.New("transient")
	cases := []struct {
		name      string
		cfg       retry.Config
		failures  int
		wantCalls int
		wantErr   bool
	}{
		{"succeeds after retries", retry.Config{Attempts: 3}, 2, 3, false},
		{"gives up", retry.Config{Attempts: 2}, 5, 2, true},
		{"zero config uses the default", retry.Config{}, 5, 3, true},
		{"first try succeeds", retry.Config{Attempts: 3}, 0, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, calls := flaky(c.failures, transient)
			err := retry.Retry(context.Background(), wrap(c.cfg, next))
			if *calls != c.wantCalls || (err != nil) != c.wantErr {
				t.Errorf("%d calls, err %v; want %d calls, err %v", *calls, err, c.wantCalls, c.wantErr)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	next, calls := flaky(2, errors.New("x"))
	start := time.Now()
	err := retry.Retry(context.Background(), wrap(retry.Config{Attempts: 3, Backoff: 10 * time.Millisecond}, next))
	// Waits 10ms, then 20ms.
	if d := time.Since(start); err != nil || *calls != 3 || d < 30*time.Millisecond {
		t.Errorf("err %v, %d calls in %v; want >= 30ms of backoff", err, *calls, d)
	}

	// Capped: 4 waits of 5ms = 20ms, against 5+10+20+40 = 75ms uncapped.
	next, calls = flaky(4, errors.New("x"))
	start = time.Now()
	err = retry.Retry(context.Background(), wrap(retry.Config{Attempts: 5, Backoff: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond}, next))
	if d := time.Since(start); err != nil || *calls != 5 || d < 20*time.Millisecond || d > 60*time.Millisecond {
		t.Errorf("err %v, %d calls in %v; want about 20ms of capped backoff", err, *calls, d)
	}
}

func TestStopsWhenCtxIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	next := func(context.Context) error {
		calls++
		cancel() // the caller gives up during the first attempt
		return context.Canceled
	}
	if err := retry.Retry(ctx, wrap(retry.Config{Attempts: 5}, next)); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Errorf("err %v after %d calls; want one call", err, calls)
	}

	// A done ctx also cuts a backoff wait short.
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	next2, calls2 := flaky(5, errors.New("x"))
	start := time.Now()
	retry.Retry(ctx, wrap(retry.Config{Attempts: 5, Backoff: time.Hour}, next2))
	if *calls2 != 1 || time.Since(start) > time.Second {
		t.Errorf("%d calls; want the backoff interrupted after one", *calls2)
	}
}

func TestPanicsAreNotRetried(t *testing.T) {
	// The panicked task is wrapped by a middleware's error, as when a middleware
	// between retry and the task adds context.
	panicked := &graph.TaskError{Path: "main/x", Op: "x", Err: errors.New("panic: boom"), Panic: "boom"}
	next, calls := flaky(5, &graph.TaskError{Path: "main", Middleware: "log", Err: fmt.Errorf("logged: %w", panicked)})
	retry.Retry(context.Background(), wrap(retry.Config{Attempts: 5}, next))
	if *calls != 1 {
		t.Errorf("%d calls; a panic must not be retried", *calls)
	}
}

func TestWhen(t *testing.T) {
	permanent := errors.New("permanent")
	only := retry.When(func(err error) bool { return !errors.Is(err, permanent) })
	next, calls := flaky(5, permanent)
	only(context.Background(), wrap(retry.Config{Attempts: 5}, next))
	if *calls != 1 {
		t.Errorf("%d calls; a non-retryable error must stop at once", *calls)
	}
	next, calls = flaky(2, errors.New("transient"))
	if err := only(context.Background(), wrap(retry.Config{Attempts: 5}, next)); err != nil || *calls != 3 {
		t.Errorf("err %v, %d calls", err, *calls)
	}
}

type countOut struct {
	N int `graph:"n"`
}

// Through a document: the config is decoded from wrap:, durations included,
// and a failed attempt's writes are discarded.
func TestInGraph(t *testing.T) {
	calls := 0
	main := graph.NewEntry[struct{}, countOut]("main")
	op := graph.Op("flaky", func(context.Context, struct{}) (countOut, error) {
		calls++
		if calls < 3 {
			return countOut{N: -calls}, errors.New("transient")
		}
		return countOut{N: calls}, nil
	})
	doc := `
graphs:
  main:
    do:
      - r:
          wrap:
            - retry:
                attempts: 3
                backoff: 1ms
          do:
            - f:
                call: flaky
`
	p, err := graph.Compile([]byte(doc), main, op, graph.Middleware("retry", retry.Retry))
	if err != nil {
		t.Fatal(err)
	}
	out, err := graph.Run(context.Background(), p, main, struct{}{})
	if err != nil || out.N != 3 {
		t.Fatalf("n = %d, err %v", out.N, err)
	}

	bad := strings.Replace(doc, "attempts: 3", "attempts: -1", 1)
	if _, err := graph.Compile([]byte(bad), main, op, graph.Middleware("retry", retry.Retry)); err == nil || !strings.Contains(err.Error(), "attempts must be >= 0") {
		t.Errorf("negative attempts: %v", err)
	}
	unknown := strings.Replace(doc, "attempts: 3", "times: 3", 1)
	if _, err := graph.Compile([]byte(unknown), main, op, graph.Middleware("retry", retry.Retry)); err == nil || !strings.Contains(err.Error(), "times") {
		t.Errorf("unknown field: %v", err)
	}
}
