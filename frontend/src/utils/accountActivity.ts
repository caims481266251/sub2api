import type { AccountQualityState, AccountRecentRequest } from '@/api/admin/accountActivity'
import { firstTokenSeverity, type LatencySeverity } from './latencyHealth'

export function recentRequestTone(request: AccountRecentRequest): LatencySeverity | 'unknown' {
  if (request.outcome === 'failure') return 'critical'
  if (request.outcome !== 'success') return 'unknown'
  const first = request.first_token_ms
  return first == null || !Number.isFinite(first) || first < 0 ? 'good' : firstTokenSeverity(first)
}

export function qualityBadgeClass(state?: AccountQualityState): string {
  if (state === 'passed') return 'badge-success'
  if (state === 'failed') return 'badge-danger'
  if (state === 'error' || state === 'unknown' || state === 'paused') return 'badge-warning'
  if (state === 'running' || state === 'queued') return 'badge-primary'
  return 'badge-gray'
}

export function activityDuration(ms?: number | null): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—'
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`
}
