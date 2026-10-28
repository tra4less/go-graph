package graph

import (
	"time"

	"gopkg.in/yaml.v3"
)

// AST of a graph document, produced by parse and consumed by the compiler.
// Positions are kept for error reporting.

type taskKind int

const (
	kindCall taskKind = iota + 1
	kindDo
	kindFork
	kindSwitch
	kindRun
)

// pos is a position in the document of module moduleID ("" for the root).
type pos struct {
	moduleID  string
	line, col int
}

type docAST struct {
	imports []*importDecl
	graphs  []*graphAST
}

type graphAST struct {
	moduleID string // id of the module defining it
	name     string
	pos      pos
	wrap     []mwRef
	do       []*taskAST

	calls []*taskAST // its run tasks whose target resolved, set by load
}

// qualified names the graph across modules, for messages.
func (g *graphAST) qualified() string {
	if g.moduleID == "" {
		return g.name
	}
	return g.moduleID + "." + g.name
}

type taskAST struct {
	name string
	pos  pos
	kind taskKind

	call    string
	with    *yaml.Node
	in, out map[string]string

	do       []*taskAST // kindDo
	branches []*taskAST // kindFork
	cases    []*caseAST // kindSwitch
	graph    string     // kindRun: the name as written
	target   *graphAST  // kindRun: resolved by load; nil if unresolved
	path     string     // kindRun: task path, set by load for messages
	wrap     []mwRef    // kindDo only

	timeout    time.Duration
	optional   bool
	background bool
	cond       string // `if:`; empty when absent
	condPos    pos
}

type caseAST struct {
	name    string
	pos     pos
	when    string // empty only for the default case
	whenPos pos
	do      []*taskAST
}

type mwRef struct {
	name string
	pos  pos
	cfg  *yaml.Node
}

// defaultCase is the name of a switch's fallback case.
const defaultCase = "default"
