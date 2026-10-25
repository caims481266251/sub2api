import type { OpsErrorLog } from '@/api/admin/ops'

// Historical in-band failures can also have HTTP 200. Only the server's
// explicit recovered telemetry marker makes a successful recovery row.
export function isRecoveredUpstreamLog(log: OpsErrorLog | null): boolean {
  if (!log || log.status_code < 200 || log.status_code >= 300) return false
  if (log.phase !== 'upstream' && log.phase !== 'account_auth') return false
  return /^(Recovered upstream error|Recovered account authentication failure)(?:\b|$)/.test(log.message || '')
}
