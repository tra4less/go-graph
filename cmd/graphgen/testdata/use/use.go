// Package use is a fixture for graphgen tests: the package of the generated file.
package use

import (
	"context"

	"github.com/tra4less/go-graph/cmd/graphgen/testdata/dep"
)

// Local is a local receiver; its name collides with nothing.
type Local struct{}

func (l *Local) Op(context.Context, dep.In) (dep.Out, error) { return dep.Out{}, nil }

// r, errors and graph are top-level names, so the generated parameter and
// imports must avoid them.
var (
	r      = 0
	errors = 0
	graph  = 0
)

var _, _, _ = r, errors, graph
