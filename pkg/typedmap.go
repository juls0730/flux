package pkg

import "sync"

type TypedMap[K comparable, V any] struct {
	internal sync.Map
}

func (m *TypedMap[K, V]) Load(key K) (V, bool) {
	val, ok := m.internal.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	return val.(V), true
}

func (m *TypedMap[K, V]) Store(key K, value V) {
	m.internal.Store(key, value)
}

func (m *TypedMap[K, V]) Delete(key K) {
	m.internal.Delete(key)
}

func (m *TypedMap[K, V]) Range(f func(key K, value V) bool) {
	m.internal.Range(func(k, v any) bool {
		return f(k.(K), v.(V))
	})
}
