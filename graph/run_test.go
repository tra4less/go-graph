package graph_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

func TestPanic(t *testing.T) {
	main := graph.NewEntry[NoIn, ABOut]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do: [ { boom: { call: put, with: { panic: true } } } ]
`)
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var ne *graph.TaskError
	if !errors.As(err, &ne) || ne.Panic != "boom" || len(ne.Stack) == 0 || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("err %v", err)
	}
}

func TestConcurrentRuns(t *testing.T) {
	p := mustCompile(t, optsOf(baseOptions()...), answerDoc)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			uid := fmt.Sprintf("u%d", i)
			out, err := graph.Run(context.Background(), p, Main, MainIn{UID: uid, Query: "q"})
			if err != nil {
				t.Error(err)
				return
			}
			if out.Answer != "plain:"+uid {
				t.Errorf("got %q", out.Answer)
			}
		})
	}
	wg.Wait()
}

// Regression tests for the semantics that are easy to get subtly wrong:
// isolation of failed or abandoned work, cancellation, and middleware.

// deafOp sleeps ignoring ctx and reports through running whether it is still

// inside.

// A middleware that calls next on another goroutine and returns without

// waiting violates the contract; the outcome must still be deterministic.

// An operator calling runtime.Goexit (t.FailNow, say) on a goroutine of the
// scheduler must fail the run, not strand it.
func TestGoexitDoesNotHang(t *testing.T) {
	exit := graph.Op("exit", func(context.Context, struct{}) (struct{}, error) {
		runtime.Goexit()
		return struct{}{}, nil
	})
	for name, task := range map[string]string{
		"fork":    `fork: [ { x: { call: exit } }, { y: { call: put } } ]`,
		"timeout": `call: exit, timeout: 1s`,
	} {
		main := graph.NewEntry[NoIn, struct{}]("main")
		p := mustCompile(t, behaviour(t, main, exit), "graphs:\n  main:\n    do:\n      - t: { "+task+" }\n")
		done := make(chan error, 1)
		go func() {
			_, err := graph.Run(context.Background(), p, main, NoIn{})
			done <- err
		}()
		select {
		case err := <-done:
			var te *graph.TaskError
			if !errors.As(err, &te) || !strings.Contains(err.Error(), "Goexit") {
				t.Errorf("%s: want a TaskError about Goexit, got %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Run hangs", name)
		}
	}
}
