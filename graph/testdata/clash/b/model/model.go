// Package model is a test fixture: its CEL name model.Info clashes with
// that of testdata/clash/a/model.
package model

type Info struct{ Level string }

// Holder reaches Info only through a field.
type Holder struct{ Info *Info }
