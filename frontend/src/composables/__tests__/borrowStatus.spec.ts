import { describe, expect, it } from 'vitest'
import { borrowProbeState, borrowReady, borrowSeconds } from '../borrowStatus'
import { observeBorrowAvailability } from '../borrowAvailability'
import type { BorrowTarget } from '@/api/admin/ticketBorrowing'

const now = Date.parse('2026-10-11T10:00:00Z')
const target = (patch: Partial<BorrowTarget> = {}): BorrowTarget => ({ account_id: 1, name: 'target', enabled: true, revision: 1, ready: true, expires_at: new Date(now + 180000).toISOString(), usable_until: new Date(now + 60000).toISOString(), server_now: new Date(now).toISOString(), next_attempt_at: new Date(now - 1000).toISOString(), next_probe_at: new Date(now - 1000).toISOString(), phase: 'waiting', reason: 'target_ticket_changed', maintenance_state: 'search', ...patch })

describe('borrowing status and availability presentation', () => {
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
