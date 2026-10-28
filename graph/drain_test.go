package graph_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

func TestDrainConcurrentWithRuns(t *testing.T) {
	main := graph.NewEntry[NoIn, struct{}]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - bg: { call: put, with: { sleep: 1ms }, background: true, timeout: 1s }
`)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := graph.Run(ctx, p, main, NoIn{}); err != nil {
					t.Error(err)
				}
			}
		})
		wg.Go(func() {
			for range 50 {
				if err := p.Drain(ctx); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if err := p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.Drain(ctx); err != nil {
		t.Errorf("idle Drain with a done ctx: %v", err)
	}
}

// Drain with a done ctx is a non-blocking probe: nil when idle, the ctx's
// error while work is in flight.
func TestDrainProbe(t *testing.T) {
	main := graph.NewEntry[NoIn, struct{}]("main")
	release := make(chan struct{})
	p := mustCompile(t, behaviour(t, main, graph.Op("block", func(context.Context, struct{}) (struct{}, error) {
		<-release
		return struct{}{}, nil
	})), "graphs:\n  main:\n    do: [ { bg: { call: block, background: true, timeout: 1s } } ]\n")
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := graph.Run(context.Background(), p, main, NoIn{}); err != nil {
		t.Fatal(err)
	}
	for range 20 { // select must not pick an idle answer at random
		if err := p.Drain(done); !errors.Is(err, context.Canceled) {
			t.Fatalf("busy probe: %v", err)
		}
	}
	close(release)
	if err := p.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(done); err != nil {
		t.Errorf("idle probe: %v", err)
	}
}

func TestDrainWaitsForAbandonedWork(t *testing.T) {
	docs := map[string]string{
		"background timeout": "      - bg: { call: deaf, background: true, timeout: 5ms }\n",
		"main timeout":       "      - slow: { call: deaf, timeout: 5ms, optional: true }\n",
		"fork sibling": "      - fan:\n          fork:\n" +
			"              - slow: { call: deaf }\n" +
			"              - bad: { call: put, with: { val: x, fail: true, sleep: 5ms } }\n",
	}
	for name, tasks := range docs {
		t.Run(name, func(t *testing.T) {
			var running atomic.Int32
			main := graph.NewEntry[NoIn, struct{}]("main")
			p := mustCompile(t, behaviour(t, main, deafOp(&running, 60*time.Millisecond)),
				"graphs:\n  main:\n    do:\n"+tasks)
			graph.Run(context.Background(), p, main, NoIn{})
			if err := p.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := running.Load(); n != 0 {
				t.Errorf("Drain returned with %d operator(s) still running", n)
			}
		})
	}
}
