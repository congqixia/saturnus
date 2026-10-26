// Package keylock provides cancellable, process-local keyed locks.
package keylock

import (
	"context"
	"sync"
)

type entry struct {
	gate chan struct{}
	refs int
}

// Map is ready to use without initialization. Idle entries are removed.
type Map struct {
	mu      sync.Mutex
	entries map[string]*entry
}

func (m *Map) Lock(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[string]*entry)
	}
	e := m.entries[key]
	if e == nil {
		e = &entry{gate: make(chan struct{}, 1)}
		e.gate <- struct{}{}
		m.entries[key] = e
	}
	e.refs++
	m.mu.Unlock()
	releaseRef := func() {
		m.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(m.entries, key)
		}
		m.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case <-e.gate:
		if err := ctx.Err(); err != nil {
			e.gate <- struct{}{}
			releaseRef()
			return nil, err
		}
		return func() { e.gate <- struct{}{}; releaseRef() }, nil
	}
}
