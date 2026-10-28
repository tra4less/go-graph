package graph_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

type reqIDKey struct{}

// A background task runs alongside the main path, never writes back, and
// is finished by the time Run returns; its failure is logged, not returned.
func TestBackground(t *testing.T) {
	logs := captureLog(t)
	var bgDone, fgDone atomic.Int64 // UnixNano when each finished
	// The output cannot include b: a background task never writes back, so
	// Compile rejects an output only it would produce.
	main := graph.NewEntry[NoIn, struct {
		A string `graph:"a"`
	}]("main")
	p := mustCompile(t, behaviour(t, main,
		graph.Op("slow_bg", func(ctx context.Context, _ struct{}) (struct {
			B string `graph:"b"`
		}, error) {
			time.Sleep(30 * time.Millisecond)
			bgDone.Store(time.Now().UnixNano())
			return struct {
				B string `graph:"b"`
			}{"bg"}, nil
		}),
		graph.Op("fg", func(context.Context, struct{}) (struct {
			A string `graph:"a"`
		}, error) {
			fgDone.Store(time.Now().UnixNano())
			return struct {
				A string `graph:"a"`
			}{"fg"}, nil
		}),
	), `
graphs:
  main:
    do:
      - bg:
          call: slow_bg
          background: true
      - bgfail:
          call: put
          with: { val: x, fail: true }
          background: true
      - fg:
          call: fg
`)
	start := time.Now()
	out, err := graph.Run(context.WithValue(context.Background(), reqIDKey{}, "r1"), p, main, NoIn{})
	if err != nil {
		t.Fatal(err)
	}
	// Run waited for the background task (no Drain needed)...
	if bgDone.Load() == 0 || time.Since(start) < 30*time.Millisecond {
		t.Fatal("Run returned before its background task finished")
	}
	// ...but the main path did not: fg finished first.
	if fgDone.Load() >= bgDone.Load() {
		t.Error("the main path waited for the background task")
	}
	if out.A != "fg" {
		t.Errorf("a = %q", out.A)
	}
	recs := logs.records()
	if len(recs) != 1 || recs[0].attrs["path"] != "main/bgfail" || recs[0].attrs["err"] != "failed x" {
		t.Fatalf("logged %v", recs)
	}
	// Logged with the run's values, so the log line carries the request id.
	if got := recs[0].ctx.Value(reqIDKey{}); got != "r1" {
		t.Errorf("log ctx value %v", got)
	}
}

// A background task is cancelled with the run's context, not with the
// region that started it.
func TestBackgroundContext(t *testing.T) {
	var afterRegion, afterCaller atomic.Value
	region := graph.NewEntry[NoIn, struct{}]("region")
	caller := graph.NewEntry[NoIn, struct{}]("caller")
	p := mustCompile(t, behaviour(t, region, caller,
		graph.Op("outlive_region", func(ctx context.Context, _ struct{}) (struct{}, error) {
			time.Sleep(30 * time.Millisecond) // the region's 10ms timeout has expired by now
			afterRegion.Store(fmt.Sprint(ctx.Err()))
			return struct{}{}, nil
		}),
		graph.Op("wait_cancel", func(ctx context.Context, _ struct{}) (struct{}, error) {
			<-ctx.Done()
			afterCaller.Store(fmt.Sprint(context.Cause(ctx)))
			return struct{}{}, ctx.Err()
		}),
	), `
graphs:
  region:
    do:
      - r:
          timeout: 10ms
          do:
            - bg:
                call: outlive_region
                background: true
  caller:
    do:
      - bg:
          call: wait_cancel
          background: true
`)
	if _, err := graph.Run(context.Background(), p, region, NoIn{}); err != nil {
		t.Fatal(err)
	}
	if got := afterRegion.Load(); got != "<nil>" {
		t.Errorf("background ctx after its region ended: %v, want not cancelled", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := graph.Run(ctx, p, caller, NoIn{}); err != nil {
		t.Fatal(err)
	}
	if got := afterCaller.Load(); got != "context deadline exceeded" {
		t.Errorf("background ctx after the caller's deadline: %v, want cancelled", got)
	}
}

func TestBackgroundReadsSnapshot(t *testing.T) {
	seen := make(chan string, 1)
	main := graph.NewEntry[AIn, ABOut]("main")
	cat := behaviour(t, main, graph.Op("peek", func(_ context.Context, in struct {
		A string `graph:"a"`
	}) (struct{}, error) {
		time.Sleep(10 * time.Millisecond) // let the main path move on
		seen <- in.A
		return struct{}{}, nil
	}))
	p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - bg: { call: peek, background: true, timeout: 1s }
      - overwrite: { call: put, with: { key: a, val: new } }
`)
	if _, err := graph.Run(context.Background(), p, main, AIn{A: "old"}); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "old" {
		t.Errorf("background task saw %q, want the value at detach time", got)
	}
}
