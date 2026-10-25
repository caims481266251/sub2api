import type { BorrowRouteStatus, BorrowTarget } from '@/api/admin/ticketBorrowing'

export function borrowSeconds(until: string | undefined, now: number): number {
  const ms = until ? Date.parse(until) - now : 0
  return Number.isFinite(ms) ? Math.max(0, Math.ceil(ms / 1000)) : 0
}

export function borrowReady(target: BorrowTarget, now: number): boolean {
  if (target.business_routes !== undefined) {
    return target.enabled && target.business_routes.some(route => borrowRouteAccepting(route, now))
  }
  return target.enabled && target.ready && borrowSeconds(target.usable_until ?? target.expires_at, now) > 0
}

export function borrowRouteAccepting(route: BorrowRouteStatus, now: number): boolean {
  return borrowRouteState(route, now) === 'accepting'
}

// Match the admission snapshot, then advance time-sensitive states locally
// between refreshes. Occupied leases and historical verdicts are not readiness.
export function borrowRouteState(route: BorrowRouteStatus, now: number, enabled = true) {
  const reason = route.unavailable_reason
  if (route.state === 'invalid' || reason === 'invalid') return 'invalid'
  if (route.state === 'expired' || borrowSeconds(route.usable_until, now) === 0) return 'expired'
  if (!enabled) return 'unavailable'
  if (route.state === 'cooling' || reason === 'cooling') return 'cooling'
  if (reason === 'account_unavailable' || reason === 'account_cooling') return reason
  if (route.state === 'pending_probe' || reason === 'pending_probe') return route.checking ? 'checking' : 'pending_probe'
  if (route.state === 'pending_quality' || reason === 'pending_quality' ||
      (route.state === 'normal' && (route.quality_valid_until || route.quality) &&
       (borrowSeconds(route.quality_valid_until, now) === 0 || (route.quality && route.quality.status !== 'passed')))) {
    return route.checking ? 'checking' : 'pending_quality'
  }
  if (reason === 'account_busy' || (route.usable && route.available_slots === 0)) return 'busy'
  if (!route.usable) return 'unavailable'
  return route.inventory_eligible ? 'accepting' : 'boundOnly'
}

export function borrowAccountCapacity(target: BorrowTarget) {
  const business = target.business_inflight ?? (target.business_routes ?? []).reduce((sum, route) => sum + route.inflight, 0)
  const background = target.inflight ?? 0
  const occupied = business + background
  const limit = target.business_concurrency == null ? undefined : Math.max(1, target.business_concurrency)
  return { business, background, occupied, limit, remaining: limit == null ? undefined : Math.max(0, limit - occupied) }
}

export function borrowRouteQualityState(route: BorrowRouteStatus, now: number) {
  const quality = route.quality
  if (!quality || quality.status === 'pending') return 'pending'
  if (quality.status === 'passed') {
    return route.state === 'normal' && borrowSeconds(route.usable_until, now) > 0 &&
      borrowSeconds(route.quality_valid_until, now) > 0 ? 'passed' : 'stale'
  }
  if (quality.reason === 'route_quality_model_mismatch') return 'model_mismatch'
  if (quality.reason === 'route_quality_failed' || (!quality.reason && quality.answer)) return 'failed'
  return 'incomplete'
}

export function borrowRouteCounts(target: BorrowTarget, now: number) {
  const routes = target.business_routes ?? []
  const states = routes.map(route => borrowRouteState(route, now, target.enabled))
  return {
    accepting: states.filter(state => state === 'accepting').length,
    busy: states.filter(state => state === 'busy').length,
    pending: states.filter(state => ['pending_quality', 'pending_probe', 'checking'].includes(state)).length,
    cooling: states.filter(state => state === 'cooling').length,
    unavailable: states.filter(state => ['account_cooling', 'account_unavailable', 'unavailable', 'boundOnly'].includes(state)).length,
    expired: states.filter(state => state === 'expired').length,
    invalid: states.filter(state => state === 'invalid').length,
    stored: target.stored_inventory_count ?? states.filter(state => !['expired', 'invalid'].includes(state)).length,
    desired: target.desired_inventory ?? 128
  }
}

export function borrowTargetAdmissionState(target: BorrowTarget, now: number) {
  if (!target.enabled) return 'disabled'
  if (borrowReady(target, now)) return 'ready'
  return borrowRouteCounts(target, now).busy > 0 ? 'busy' : 'noRoute'
}

export function borrowAdmissionDeadline(target: BorrowTarget, now: number): string | undefined {
  if (target.business_routes === undefined) return target.usable_until ?? target.expires_at
  return target.business_routes.filter(route => borrowRouteAccepting(route, now))
    .map(route => route.usable_until).sort((a, b) => Date.parse(b) - Date.parse(a))[0]
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
