import type { BorrowRouteStatus, BorrowTarget } from '@/api/admin/ticketBorrowing'

export function borrowSeconds(until: string | undefined, now: number): number {
  const ms = until ? Date.parse(until) - now : 0
  return Number.isFinite(ms) ? Math.max(0, Math.ceil(ms / 1000)) : 0
}

export function borrowReady(target: BorrowTarget, now: number): boolean {
  return target.enabled && target.ready && borrowSeconds(target.usable_until ?? target.expires_at, now) > 0
}

export function borrowRouteAccepting(route: BorrowRouteStatus, now: number): boolean {
  return route.usable && route.inventory_eligible && borrowSeconds(route.usable_until, now) > 0
}

// Current scheduling state is independent of the last completed probe's verdict.
export function borrowProbeState(target: BorrowTarget, running: boolean, now: number) {
  if (!target.enabled || !running) {
    return { phase: (target.inflight ?? 0) > 0 ? 'draining' : target.enabled ? 'paused' : 'disabled', reason: '', until: undefined }
  }
  if ((target.inflight ?? 0) > 0 || target.blocked_reason === 'batch_running') {
    return { phase: 'probing', reason: '', until: undefined }
  }
  if (borrowSeconds(target.upstream_wait_until, now) > 0) {
    return { phase: 'upstream', reason: target.upstream_wait_reason || 'target_upstream_waiting', until: target.upstream_wait_until }
  }
  if (borrowSeconds(target.budget_wait_until, now) > 0 || ['source_request_budget', 'target_request_budget'].includes(target.blocked_reason ?? '')) {
    return { phase: 'budget', reason: target.blocked_reason || 'target_request_budget', until: target.budget_wait_until }
  }
  if (target.blocked_reason && !['borrow_acquisition_paused', 'batch_running'].includes(target.blocked_reason)) {
    return { phase: 'unavailable', reason: target.blocked_reason, until: undefined }
  }
  return { phase: 'waiting', reason: '', until: target.next_probe_at || target.next_attempt_at }
}
