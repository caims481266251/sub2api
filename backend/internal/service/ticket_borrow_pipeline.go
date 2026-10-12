package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

type borrowProbeCandidate struct {
	source  int64
	cookie  *http.Cookie
	expires time.Time
	details *BorrowCandidateDetails
	routeID string
}
type borrowProbeVerification struct {
	candidate         borrowProbeCandidate
	result            *OpenAICodexStateProbeResult
	reason            string
	terminal, scanned bool
}

// All callbacks except acquire/verify execute on the coordinator goroutine.
// C queued-or-acquiring candidates plus C verifications bound all memory and
// speculative work. The database independently enforces actual HTTP capacity.
type borrowCandidatePipeline struct {
	capacity   int
	continuous bool
	sources    []BorrowSource
	needed     func() bool
	acquire    func(BorrowSource) (borrowProbeCandidate, error)
	verify     func(borrowProbeCandidate) borrowProbeVerification
	consume    func(borrowProbeVerification) bool
	priorities func() map[string]time.Time
	selected   func(borrowProbeCandidate, bool, bool)
	discard    func(borrowProbeVerification)
}

// FIFO within each class, with one demoted candidate after three normal picks
// while both classes are pending. No alternate candidate means no waiting.
func selectBorrowCandidate(queue []borrowProbeCandidate, priorities map[string]time.Time, now time.Time, streak *int) (int, bool) {
	normal, demoted := -1, -1
	for i, c := range queue {
		if priorities[c.details.Gateway].After(now) {
			if demoted < 0 {
				demoted = i
			}
		} else if normal < 0 {
			normal = i
		}
	}
	if demoted >= 0 && (normal < 0 || *streak >= 3) {
		*streak = 0
		return demoted, true
	}
	if demoted >= 0 {
		(*streak)++
	} else {
		*streak = 0
	}
	return normal, false
}

func (p borrowCandidatePipeline) run(ctx context.Context, cancel context.CancelFunc) {
	c := max(1, p.capacity)
	if len(p.sources) == 0 {
		cancel()
		return
	}
	if !p.continuous {
		c = min(c, len(p.sources))
	}
	type event struct {
		source    BorrowSource
		candidate borrowProbeCandidate
		err       error
		result    *borrowProbeVerification
	}
	events := make(chan event, 2*c)
	var workers sync.WaitGroup
	queue := make([]borrowProbeCandidate, 0, c)
	acquiring, verifying, attempted, cursor, streak := 0, 0, 0, 0, 0
	deferred := 0
	closed, stopped := false, false
	busy := map[int64]bool{}
	retryAt := map[int64]time.Time{}
	defer func() {
		cancel()
		workers.Wait()
		for _, item := range queue {
			p.discard(borrowProbeVerification{candidate: item})
		}
		for len(events) > 0 {
			ev := <-events
			if ev.result != nil {
				if !stopped {
					stopped = !p.consume(*ev.result)
				} else {
					p.discard(*ev.result)
				}
			} else if ev.err == nil {
				p.discard(borrowProbeVerification{candidate: ev.candidate})
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	handle := func(ev event) bool {
		if ev.result != nil {
			verifying--
			stopped = !p.consume(*ev.result)
			return !stopped
		}
		acquiring--
		delete(busy, ev.source.ID)
		if ev.err == nil {
			// Source event persistence can finish out of order. Preserve Cookie
			// acquisition order within each gateway priority class.
			at := len(queue)
			if observed := ev.candidate.details.CookieObservedAt; observed != nil {
				for i, item := range queue {
					if prior := item.details.CookieObservedAt; prior != nil && observed.Before(*prior) {
						at = i
						break
					}
				}
			}
			queue = append(queue, ev.candidate)
			copy(queue[at+1:], queue[at:len(queue)-1])
			queue[at] = ev.candidate
			attempted++
			deferred = 0
		} else {
			retryAt[ev.source.ID] = time.Now().Add(time.Second)
			if errors.Is(ev.err, errBorrowSourceDeferred) {
				deferred++
			} else {
				attempted++
				deferred = 0
			}
			if deferred >= len(p.sources) {
				closed = true
			}
		}
		return true
	}
	for {
		if ctx.Err() != nil {
			return
		}
		// Consume completed verdicts before selecting another candidate, so a new
		// success or failure feedback is visible at the next scheduling decision.
		for len(events) > 0 {
			if !handle(<-events) {
				return
			}
		}
		if len(queue) > 0 && verifying < c {
			priorities := p.priorities()
			for len(queue) > 0 && verifying < c {
				index, demoted := selectBorrowCandidate(queue, priorities, time.Now(), &streak)
				item := queue[index]
				queue = append(queue[:index], queue[index+1:]...)
				p.selected(item, demoted, index > 0 && !demoted)
				verifying++
				workers.Add(1)
				go func() { defer workers.Done(); result := p.verify(item); events <- event{result: &result} }()
			}
		}
		if !closed && acquiring+len(queue) < c {
			if !p.needed() {
				closed = true
			}
		}
		for !closed && acquiring+len(queue) < c && (p.continuous || attempted+acquiring < c) {
			found := -1
			for step := 0; step < len(p.sources); step++ {
				index := (cursor + step) % len(p.sources)
				src := p.sources[index]
				if (src.ID == 0 || !busy[src.ID]) && !retryAt[src.ID].After(time.Now()) {
					found = index
					break
				}
			}
			if found < 0 {
				break
			}
			src := p.sources[found]
			cursor = (found + 1) % len(p.sources)
			if src.ID > 0 {
				busy[src.ID] = true
			}
			acquiring++
			workers.Add(1)
			go func() {
				defer workers.Done()
				item, err := p.acquire(src)
				events <- event{source: src, candidate: item, err: err}
			}()
		}
		if !p.continuous && attempted >= c {
			closed = true
		}
		if closed && acquiring == 0 && verifying == 0 && len(queue) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if !handle(ev) {
				return
			}
		case <-ticker.C:
		}
	}
}
