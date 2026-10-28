package graph_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

func TestForkJoinAndOptional(t *testing.T) {
	main := graph.NewEntry[AIn, JoinedOut]("main")
	calls, rec := recordCalls()
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - fan:
          fork:
              - s1: { call: put_shared, with: { val: first, sleep: 30ms } }
              - lostb: { call: put, with: { key: b, val: x, fail: true }, optional: true }
      - join: { call: join }
`, rec...)
	out, err := graph.Run(context.Background(), p, main, AIn{A: "in"})
	if err != nil {
		t.Fatal(err)
	}
	// The fork waits for the slow branch; the lost one leaves b absent.
	if out.Joined != "in|-|first" {
		t.Errorf("joined %q", out.Joined)
	}
	if calls.err("main/fan/lostb") == nil {
		t.Error("lostb did not fail: the run succeeded for the wrong reason")
	}
}

func TestForkFailFast(t *testing.T) {
	var cancelled atomic.Bool
	main := graph.NewEntry[NoIn, struct{}]("main")
	cat := behaviour(t, main, graph.Op("wait", func(ctx context.Context, _ struct{}) (struct{}, error) {
		<-ctx.Done()
		cancelled.Store(true)
		return struct{}{}, ctx.Err()
	}))
	p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - fan:
          fork:
              - slow: { call: wait }
              - bad: { call: put, with: { val: x, fail: true, sleep: 20ms } }
`)
	start := time.Now()
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var ne *graph.TaskError
	if !errors.As(err, &ne) || ne.Path != "main/fan/bad" || ne.Op != "put" {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("fork did not fail fast")
	}
	deadline := time.Now().Add(time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cancelled.Load() {
		t.Error("sibling was not cancelled")
	}
}

func TestPanicInForkBranch(t *testing.T) {
	main := graph.NewEntry[NoIn, struct{}]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - fan:
          fork:
              - ok: { call: put, with: { key: b, val: v } }
              - boom: { call: put_shared, with: { panic: true } }
`)
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var ne *graph.TaskError
	if !errors.As(err, &ne) || ne.Panic == nil || ne.Path != "main/fan/boom" {
		t.Fatalf("err %v", err)
	}
}
