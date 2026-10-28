package graph_test

import (
	"context"
	"errors"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

func TestTimeout(t *testing.T) {
	hard := graph.NewEntry[NoIn, ABOut]("hard")
	deaf := graph.NewEntry[NoIn, ABOut]("deaf")
	p := mustCompile(t, behaviour(t, hard, deaf), `
graphs:
  hard:
    do:
      - slow: { call: put, with: { key: b, val: late, sleep: 1s }, timeout: 20ms }
  deaf:
    do:
      - slow: { call: put, with: { key: b, val: late, sleep: 80ms, deaf: true }, timeout: 20ms, optional: true }
`)
	_, err := graph.Run(context.Background(), p, hard, NoIn{})
	var ne *graph.TaskError
	if !errors.Is(err, graph.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &ne) || ne.Path != "hard/slow" {
		t.Fatalf("hard: %v", err)
	}

	// An operator that ignores ctx is abandoned; its late result is discarded.
	start := time.Now()
	out, err := graph.Run(context.Background(), p, deaf, NoIn{})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 60*time.Millisecond {
		t.Errorf("Run took %v: it waited for the abandoned operator", d)
	}
	if out.B.OK {
		t.Errorf("abandoned write leaked: %q", out.B.V)
	}
}

func TestOptionalDoesNotSwallowCallerCancellation(t *testing.T) {
	main := graph.NewEntry[NoIn, ABOut]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - slow: { call: put, with: { key: b, val: v, sleep: 1s }, optional: true }
`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := graph.Run(ctx, p, main, NoIn{})
	var te *graph.TaskError
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, graph.ErrTimeout) || !errors.As(err, &te) || te.Path != "main/slow" {
		t.Fatalf("want the caller's deadline at main/slow, got %v", err)
	}
}

func TestTimeoutCauseVisibleToOperator(t *testing.T) {
	causes := make(chan error, 1)
	main := graph.NewEntry[NoIn, struct{}]("main")
	cat := behaviour(t, main, graph.Op("wait", func(ctx context.Context, _ struct{}) (struct{}, error) {
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return struct{}{}, ctx.Err()
	}))
	p := mustCompile(t, cat, `
graphs:
  main:
    do: [ { w: { call: wait, timeout: 10ms } } ]
`)
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	if cause := <-causes; !errors.Is(err, graph.ErrTimeout) || !errors.Is(cause, graph.ErrTimeout) {
		t.Fatalf("err %v, cause %v", err, cause)
	}
}

func TestAbandonedWorkIsDiscarded(t *testing.T) {
	main := graph.NewEntry[NoIn, ABOut]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - slow:
          timeout: 10ms
          optional: true
          do:
            - deaf: { call: put, with: { key: b, val: late, sleep: 40ms, deaf: true } }
            - after: { call: put, with: { key: b, val: later } }
      - wait: { call: put, with: { sleep: 80ms } }   # abandoned work finishes meanwhile
`)
	out, err := graph.Run(context.Background(), p, main, NoIn{})
	if err != nil {
		t.Fatal(err)
	}
	// Both deaf's late write and after's write land in the abandoned overlay.
	if out.B.OK {
		t.Errorf("abandoned write leaked: %q", out.B.V)
	}
}
