// Package dep is a fixture for graphgen tests: its name is not the last
// element of its import path, and clashes with testdata/dep.
package dep

import "context"

type In struct{}
type Out struct{}

func Other(context.Context, In) (Out, error) { return Out{}, nil }
