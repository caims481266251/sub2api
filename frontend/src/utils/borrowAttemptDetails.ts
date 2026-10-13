import type { TimingAttempt, BorrowRecovery } from '@/api/admin/usageTiming'
export interface BorrowAttemptDetail { attempt: TimingAttempt; recovery?: BorrowRecovery }
// Historical error rows can contain malformed or truncated JSON. Show only
// structured observations, never infer a route from account selection alone.
export function borrowAttemptDetails(raw: string | null | undefined): BorrowAttemptDetail[] {
  if (!raw) return []
  try {
    const events: unknown = JSON.parse(raw)
    if (!Array.isArray(events)) return []
    const byAttempt = new Map<number, BorrowAttemptDetail>()
    for (const event of events.slice(0, 256)) {
      const a = event?.attempt
      if (!a || a.kind !== 'egress' || typeof a.borrow_route_id !== 'string' || !a.borrow_route_id || !Number.isFinite(a.number) || !Number.isFinite(a.account_id) || !a.events || typeof a.events !== 'object') continue
      byAttempt.set(a.number, { attempt: a, recovery: event.borrow_recovery })
    }
    return [...byAttempt.values()].slice(0, 32)
  } catch { return [] }
}
