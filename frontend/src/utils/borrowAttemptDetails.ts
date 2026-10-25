import type { TimingAttempt, BorrowRecovery } from '@/api/admin/usageTiming'
export interface BorrowAttemptDetail { attempt: TimingAttempt; recovery?: BorrowRecovery; outcome?: 'recovered' | 'failed' | 'canceled' }
// Historical error rows can contain malformed or truncated JSON. Show only
// structured observations, never infer a route from account selection alone.
export function borrowAttemptDetails(raw: string | null | undefined): BorrowAttemptDetail[] {
  if (!raw) return []
  try {
    const events: unknown = JSON.parse(raw)
    if (!Array.isArray(events)) return []
    const bounded = events.slice(0, 256)
    const hasBorrowing = bounded.some(event => event?.attempt?.borrow_route_id || event?.borrow_recovery)
    if (!hasBorrowing) return []
    const byAttempt = new Map<number, BorrowAttemptDetail>()
    for (const event of bounded) {
      for (const [a, final] of [[event?.attempt, false], [event?.request_outcome?.attempt, true]] as const) {
        if (!a || a.kind !== 'egress' || !Number.isFinite(a.number) || !Number.isFinite(a.account_id) || !a.events || typeof a.events !== 'object') continue
        const state = final ? event.request_outcome.state : undefined
        const outcome = state === 'recovered' || state === 'failed' || state === 'canceled' ? state : undefined
        byAttempt.set(a.number, { attempt: a, recovery: event.borrow_recovery, outcome })
      }
    }
    return [...byAttempt.values()].sort((a, b) => a.attempt.number - b.attempt.number).slice(0, 32)
  } catch { return [] }
}
