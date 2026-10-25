import { apiClient } from '../client'

export type AccountQualityState = 'untested' | 'running' | 'queued' | 'passed' | 'failed' | 'unknown' | 'error' | 'paused'

export interface AccountQualityResult {
  state: AccountQualityState
  answer?: string | null
  model: string
  checked_at: string
  duration_ms: number
  error?: string | null
  text?: string | null
}

export interface AccountQualityMonitor {
  account_id: number
  scope: string
  enabled: boolean
  model: string
  interval_minutes: number
  state: AccountQualityState
  pause_reason?: string | null
  next_due_at?: string | null
  latest_attempt?: AccountQualityResult | null
  last_valid?: AccountQualityResult | null
}

export interface AccountRecentRequest {
  attempt_id: string
  request_id?: string
  account_id: number
  source: string
  model: string
  completed_at: string
  outcome: 'success' | 'failure' | 'cancelled' | 'unknown'
  http_status: number
  first_token_ms: number | null
  duration_ms: number
  error_code?: string
  error_message?: string
}

export interface AccountActivity {
  account_id: number
  quality: AccountQualityMonitor
  recent_requests: AccountRecentRequest[] | null
}

export interface AccountQualitySettings {
  scope: string
  enabled: boolean
  model: string
  interval_minutes: number
}

export async function getAccountActivity(accountIDs: number[], signal?: AbortSignal): Promise<AccountActivity[]> {
  const { data } = await apiClient.post<{ items: AccountActivity[] }>('/admin/accounts/activity/batch', {
    account_ids: accountIDs
  }, { signal })
  return data.items ?? []
}

export async function getAccountQuality(accountID: number, signal?: AbortSignal): Promise<AccountQualityMonitor> {
  const { data } = await apiClient.get<AccountQualityMonitor>(`/admin/accounts/${accountID}/quality-monitor`, { signal })
  return data
}

export async function saveAccountQuality(accountID: number, settings: AccountQualitySettings): Promise<AccountQualityMonitor> {
  const { data } = await apiClient.put<AccountQualityMonitor>(`/admin/accounts/${accountID}/quality-monitor`, settings)
  return data
}

export async function runAccountQuality(accountID: number, scope: string): Promise<AccountQualityMonitor> {
  const { data } = await apiClient.post<AccountQualityMonitor>(`/admin/accounts/${accountID}/quality-monitor/run`, { scope })
  return data
}
