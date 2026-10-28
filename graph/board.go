package graph

import "sync"

// board is the blackboard of one run.
//
// A child board (overlay) reads through to its parent and buffers its own
// writes until commit. Work whose effects may have to be dropped (a task with
// a timeout or optional, each fork branch, each middleware attempt) writes to
// an overlay, so abandoned or failed work can never leak into the main path.
type board struct {
	parent *board
	mu     sync.RWMutex
	vals   map[string]any
}

func newBoard() *board { return &board{vals: make(map[string]any)} }

func (b *board) child() *board { return &board{parent: b, vals: make(map[string]any)} }

func (b *board) get(name string) (any, bool) {
	for cur := b; cur != nil; cur = cur.parent {
		cur.mu.RLock()
		v, ok := cur.vals[name]
		cur.mu.RUnlock()
		if ok {
			return v, true
		}
	}
	return nil, false
}

func (b *board) set(name string, v any) {
	b.mu.Lock()
	b.vals[name] = v
	b.mu.Unlock()
}

// commit publishes the overlay's writes to its parent. Locks are always taken
// child before parent, so concurrent commits cannot deadlock.
func (b *board) commit() {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.parent.mu.Lock()
	defer b.parent.mu.Unlock()
	for k, v := range b.vals {
		b.parent.vals[k] = v
	}
}

// snapshot returns a detached root board holding everything b can see now.
// Later writes to b are not visible through it, and vice versa.
func (b *board) snapshot() *board {
	var chain []*board
	for cur := b; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	s := newBoard()
	for i := len(chain) - 1; i >= 0; i-- { // root first, so nearer layers win
		cur := chain[i]
		cur.mu.RLock()
		for k, v := range cur.vals {
			s.vals[k] = v
		}
		cur.mu.RUnlock()
	}
	return s
}
