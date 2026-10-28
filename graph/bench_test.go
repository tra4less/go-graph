package graph_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
)

type benchN struct {
	N int `graph:"n"`
}

// benchOptions has one operator, "inc", which reads and rewrites n.
func benchOptions(e graph.Option) []graph.Option {
	return []graph.Option{e, graph.Op("inc", func(_ context.Context, in benchN) (benchN, error) {
		return benchN{N: in.N + 1}, nil
	})}
}

func benchRun(b *testing.B, doc string, extra ...graph.Option) {
	e := graph.NewEntry[benchN, benchN]("main")
	p, err := graph.Compile([]byte(doc), append(benchOptions(e), extra...)...)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := graph.Run(ctx, p, e, benchN{}); err != nil {
			b.Fatal(err)
		}
	}
}

func tasks(n int, attrs string) string {
	var s strings.Builder
	for i := range n {
		fmt.Fprintf(&s, "{ t%d: { call: inc%s } },", i, attrs)
	}
	return s.String()
}

func BenchmarkRunSequence10(b *testing.B) {
	benchRun(b, "graphs:\n  main:\n    do: ["+tasks(10, "")+"]\n")
}

// The cost WrapCalls adds to every call task, with a middleware that only
// calls next.
func BenchmarkRunSequence10WrapCalls(b *testing.B) {
	benchRun(b, "graphs:\n  main:\n    do: ["+tasks(10, "")+"]\n",
		graph.Middleware("nop", func(ctx context.Context, w graph.Wrap[struct{}]) error { return w.Next(ctx) }),
		graph.WrapCalls("nop"))
}

func BenchmarkRunFork4(b *testing.B) {
	var branches strings.Builder
	for i := range 4 { // fork branches must write different keys
		fmt.Fprintf(&branches, "{ t%d: { call: inc, out: { n: n%d } } },", i, i)
	}
	benchRun(b, "graphs:\n  main:\n    do: [ { f: { fork: ["+branches.String()+"] } } ]\n")
}

func BenchmarkRunTimeout(b *testing.B) {
	benchRun(b, "graphs:\n  main:\n    do: ["+tasks(1, ", timeout: 1s")+"]\n")
}

func BenchmarkRunSwitch(b *testing.B) {
	benchRun(b, `
graphs:
  main:
    do:
      - s:
          switch:
            - big: { when: "n > 10", do: [ { a: { call: inc } } ] }
            - default: { do: [ { b: { call: inc } } ] }
`)
}

func BenchmarkCompile200(b *testing.B) {
	doc := []byte("graphs:\n  main:\n    do: [" + tasks(200, "") + "]\n")
	e := graph.NewEntry[benchN, benchN]("main")
	opts := benchOptions(e)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := graph.Compile(doc, opts...); err != nil {
			b.Fatal(err)
		}
	}
}
