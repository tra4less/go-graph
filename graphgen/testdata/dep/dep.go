// Package dep is a fixture for graphgen tests: operators in another package.
package dep

import (
	"context"

	graph "github.com/tra4less/go-graph/graph"
)

type In struct {
	Q string `graph:"q"`
}
type Out struct {
	A string `graph:"a"`
}
type Cfg struct{ N int }

// CfgIn takes a node config.
type CfgIn struct {
	Cfg Cfg    `graph:",with"`
	Q   string `graph:"q"`
}

// Client is a dependency-holding receiver.
type Client struct{}

func (*Client) Search(context.Context, In) (Out, error) { return Out{}, nil }
func (Client) Value(context.Context, In) (Out, error)   { return Out{}, nil }
func Free(context.Context, CfgIn) (Out, error)          { return Out{}, nil }
func OldWith(context.Context, Cfg, In) (Out, error)     { return Out{}, nil }
func Mw(context.Context, graph.Wrap[struct{}]) error    { return nil }

// Bad shapes.
func NoCtx(In) (Out, error)                           { return Out{}, nil }
func NotStruct(context.Context, string) (Out, error)  { return Out{}, nil }
func Generic[T any](context.Context, In) (Out, error) { return Out{}, nil }
func BadMw(context.Context, graph.Wrap[string]) error { return nil }
func hidden(context.Context, In) (Out, error)         { return Out{}, nil }

var (
	Entry    = graph.NewEntry[In, Out]("main")
	NotEntry = 1
	_        = hidden
)
