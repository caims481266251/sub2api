package service

import (
	"context"
	"sync"
)

type borrowProbeSlotsKey struct{}
type borrowProbeSlotWaiter struct {
	priority int
	ready    chan struct{}
	granted  bool
}
type borrowProbeSlots struct {
	mu               sync.Mutex
	capacity, active int
	waiting          []*borrowProbeSlotWaiter
}

// Local priority complements, but never replaces, the database concurrency
// fence. A slot covers preparation and the complete transport/body lifetime.
func withBorrowProbeSlots(ctx context.Context, capacity int) context.Context {
	return context.WithValue(ctx, borrowProbeSlotsKey{}, &borrowProbeSlots{capacity: max(1, capacity)})
}
func acquireBorrowProbeSlot(ctx context.Context, priority int) (func(), error) {
	s, _ := ctx.Value(borrowProbeSlotsKey{}).(*borrowProbeSlots)
	if s == nil {
		return func() {}, ctx.Err()
	}
	w := &borrowProbeSlotWaiter{priority: priority, ready: make(chan struct{})}
	s.mu.Lock()
	s.waiting = append(s.waiting, w)
	s.dispatch()
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			s.active--
		} else {
			for i, x := range s.waiting {
				if x == w {
					s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
					break
				}
			}
		}
		s.dispatch()
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-w.ready:
		var once sync.Once
		return func() { once.Do(func() { s.mu.Lock(); s.active--; s.dispatch(); s.mu.Unlock() }) }, nil
	}
}
func (s *borrowProbeSlots) dispatch() {
	for s.active < s.capacity && len(s.waiting) > 0 {
		index := 0
		for i, w := range s.waiting {
			if w.priority > s.waiting[index].priority {
				index = i
			}
		}
		w := s.waiting[index]
		s.waiting = append(s.waiting[:index], s.waiting[index+1:]...)
		s.active++
		w.granted = true
		close(w.ready)
	}
}
