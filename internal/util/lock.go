package util

import (
	"context"
	"fmt"
	"sync"
)

var ErrLocked = fmt.Errorf("item is locked")

type MutexLock[T comparable] struct {
	mu       sync.Mutex
	deployed map[T]context.CancelFunc
}

func NewMutexLock[T comparable]() *MutexLock[T] {
	return &MutexLock[T]{
		deployed: make(map[T]context.CancelFunc),
	}
}

func (dt *MutexLock[T]) Lock(id T, ctx context.Context) (context.Context, error) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Check if the object is locked
	if _, exists := dt.deployed[id]; exists {
		return nil, ErrLocked
	}

	// Create a context that can be cancelled
	ctx, cancel := context.WithCancel(ctx)

	// Store the cancel function
	dt.deployed[id] = cancel

	return ctx, nil
}

func (dt *MutexLock[T]) Unlock(id T) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Remove the object from the map (functionally unlocking it)
	if cancel, exists := dt.deployed[id]; exists {
		// Cancel the context
		cancel()
		// Remove from map
		delete(dt.deployed, id)
	}
}
