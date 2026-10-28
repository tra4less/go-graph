package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
)

func isUnknownEntry(err error) bool {
	return err != nil && strings.Contains(err.Error(), "entry not in program")
}

func TestUnknownEntry(t *testing.T) {
	p := mustCompile(t, optsOf(baseOptions()...), answerDoc)
	ctx := context.Background()
	// Same graph name and types, but a different Entry: not in the program.
	other := graph.NewEntry[MainIn, MainOut]("main")
	if _, err := graph.Run(ctx, p, other, MainIn{}); !isUnknownEntry(err) {
		t.Errorf("foreign entry: %v", err)
	}
	if _, err := graph.Run(ctx, nil, Main, MainIn{}); !isUnknownEntry(err) {
		t.Errorf("nil program: %v", err)
	}
}

func TestPartialOutputOnError(t *testing.T) {
	main := graph.NewEntry[NoIn, ABOut]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - first: { call: put, with: { key: a, val: done } }
      - second: { call: put, with: { key: b, val: never, fail: true } }
`)
	out, err := graph.Run(context.Background(), p, main, NoIn{})
	var te *graph.TaskError
	if !errors.As(err, &te) || te.Path != "main/second" {
		t.Fatalf("err %v: want the failure of main/second", err)
	}
	if out.A.V != "done" || out.B.OK {
		t.Errorf("out %+v: want what was written before the failure", out)
	}
}

func TestZeroEntry(t *testing.T) {
	var zero graph.Entry[NoIn, struct{}]
	if _, err := graph.Compile(nil, &zero); err == nil || !strings.Contains(err.Error(), "without a graph name") {
		t.Errorf("register zero Entry: %v", err)
	}
	p := mustCompile(t, behaviour(t, graph.NewEntry[NoIn, struct{}]("main")), "graphs:\n  main:\n    do: []\n")
	if _, err := graph.Run(context.Background(), p, &zero, NoIn{}); !isUnknownEntry(err) {
		t.Errorf("run zero Entry: %v", err)
	}
	var nilEntry *graph.Entry[NoIn, struct{}]
	if _, err := graph.Run(context.Background(), p, nilEntry, NoIn{}); !isUnknownEntry(err) {
		t.Errorf("run nil Entry: %v", err)
	}
}
