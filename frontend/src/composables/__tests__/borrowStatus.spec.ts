import { describe, expect, it } from 'vitest'
import { borrowAccountCapacity, borrowProbeState, borrowReady, borrowSeconds, borrowRouteState } from '../borrowStatus'
import { observeBorrowAvailability } from '../borrowAvailability'
import type { BorrowTarget } from '@/api/admin/ticketBorrowing'

const now = Date.parse('2026-10-11T10:00:00Z')
const target = (patch: Partial<BorrowTarget> = {}): BorrowTarget => ({ account_id: 1, name: 'target', enabled: true, revision: 1, ready: true, expires_at: new Date(now + 180000).toISOString(), usable_until: new Date(now + 60000).toISOString(), server_now: new Date(now).toISOString(), next_attempt_at: new Date(now - 1000).toISOString(), next_probe_at: new Date(now - 1000).toISOString(), phase: 'waiting', reason: 'target_ticket_changed', maintenance_state: 'search', ...patch })

describe('borrowing status and availability presentation', () => {
  it('keeps account capacity shared and never multiplies it by route count', () => {
    const current = target({ business_concurrency: 10, inflight: 2, business_inflight: 6 })
    expect(borrowAccountCapacity(current)).toEqual({ business: 6, background: 2, occupied: 8, limit: 10, remaining: 2 })
    expect(borrowAccountCapacity({ ...current, business_inflight: 10 }).remaining).toBe(0)
    expect(borrowAccountCapacity({ ...current, business_concurrency: 5 }).remaining).toBe(0)
    expect(borrowAccountCapacity({ ...current, business_concurrency: undefined }).remaining).toBeUndefined()
  })

  it('uses the same current route eligibility in target summaries and route details', () => {
    const current = target({ business_routes: [{ id: 'r', state: 'normal', usable: true, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), quality_valid_until: new Date(now + 1000).toISOString(), inflight: 0 }] } as Partial<BorrowTarget>)
    expect(borrowReady(current, now)).toBe(true)
    expect(borrowReady(current, now + 1000)).toBe(false)
    const route = current.business_routes![0]
    expect(borrowRouteState({ ...route, unavailable_reason: 'account_cooling', usable: false }, now)).toBe('account_cooling')
    expect(borrowRouteState({ ...route, unavailable_reason: 'account_unavailable', usable: false }, now)).toBe('account_unavailable')
    expect(borrowReady({ ...current, business_routes: [] }, now)).toBe(false)
  })
  it('uses admission deadlines and never derives current state from the last verdict', () => {
    expect(borrowReady(target(), now + 59999)).toBe(true)
    expect(borrowReady(target(), now + 60000)).toBe(false)
    expect(borrowProbeState(target(), true, now).phase).toBe('waiting')
    expect(borrowSeconds('invalid', now)).toBe(0)
  })
  it('hides stale due times during pause and distinguishes requests finishing', () => {
    expect(borrowProbeState(target(), false, now)).toEqual({ phase: 'paused', reason: '', until: undefined })
    expect(borrowProbeState(target({ inflight: 2 }), false, now).phase).toBe('draining')
    expect(borrowProbeState(target({ enabled: false }), true, now).phase).toBe('disabled')
    expect(borrowProbeState(target({ blocked_reason: 'source_request_budget' }), true, now).phase).toBe('budget')
  })
  it('counts only observed intervals and excludes failures, hidden gaps and disabled targets', () => {
    const t = target({ ready: false })
    let observation = observeBorrowAvailability(undefined, t, now)
    observation = observeBorrowAvailability(observation, t, now + 5000)
    expect(observation.missingMS).toBe(5000)
    observation = observeBorrowAvailability(observation, t, now + 25000)
    expect(observation.sampledMS).toBe(5000)
    observation = observeBorrowAvailability({ ...observation, lastAt: 0 }, t, now + 30000)
    expect(observation.sampledMS).toBe(5000)
    observation = observeBorrowAvailability(observation, target({ enabled: false }), now + 35000)
    expect(observation.sampledMS).toBe(5000)
  })
})
