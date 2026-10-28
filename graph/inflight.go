package graph

import (
	"context"
	"sync"
)

// inflight counts background work. Unlike sync.WaitGroup, add may be called
// concurrently with wait. The zero value is ready to use.
type inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to zero; nil while n == 0
}

func (f *inflight) add() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

func (f *inflight) done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n--
	if f.n == 0 {
		close(f.idle)
		f.idle = nil
	}
}

// wait blocks until no work is in flight or ctx is done.
func (f *inflight) wait(ctx context.Context) error {
	f.mu.Lock()
	idle := f.idle
	f.mu.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
