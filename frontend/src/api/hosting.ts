import apiClient from './client'
import type { Account, AccountUsageInfo, AccountUsageStatsResponse, ClaudeModel, TempUnschedulableStatus, Group } from '@/types'
import type { OpenAIQuotaRefreshResult, OpenAIQuotaResetResult, SyncUpstreamModelsResult } from './admin/accounts'

export interface HostedAccount {
  status?: string
  error_message?: string
  rate_limit_reset_at?: string | null
  overload_until?: string | null
  temp_unschedulable_until?: string | null
  runtime_extra?: Record<string, unknown>
  current_concurrency?: number | null
  current_rpm?: number | null
  schedulable?: boolean
  created_at?: string
  plan_type?: string
  id: number
  name: string
  email: string
  state: 'active' | 'paused' | 'login_required' | 'quota_exhausted' | 'temporarily_unavailable'
  reset_at: string | null
  last_used_at: string | null
  usage_5h_percent: number | null
  usage_7d_percent: number | null
  usage?: AccountUsageInfo
  quota_extra?: Account['extra']
  settings: HostedAccountSettings
}
export interface HostedAccountSettings {
  status?: string

  hosting_pool_id: number | null
  name: string
  concurrency: number
  priority: number
  notes?: string | null
  load_factor?: number | null
  expires_at?: number | null
  auto_pause_on_expired?: boolean
  credentials: Record<string, unknown>
  extra: Record<string, unknown>
}
export interface HostingPolicy { seat_limit: number | null; rate_multiplier: number | null }
export type HostingAccountAction = 'pause' | 'resume' | 'delete' | 'schedule-enable' | 'schedule-disable'
export interface HostingBillingEntry {
  request_id: string; input_tokens: number; output_tokens: number; cache_read_tokens: number; cache_creation_tokens: number
  input_cost: number; output_cost: number; cache_read_cost: number; cache_creation_cost: number

  id: number; created_at: string; account_name: string; key_name: string; model: string; route: 'hosted' | 'fallback'; group_name: string; reason: string; tokens: number; standard_cost: number; rate: number; actual_cost: number
}
export interface HostingBilling {
  actual_cost: number
  requests: number; tokens: number; standard_cost: number; hosting_cost: number; fallback_cost: number; items: HostingBillingEntry[]; page: number; page_size: number
}
export interface HostingOverview {
  enabled: boolean
  seat_limit: number
  used_seats: number
  rate_multiplier: number
  accounts: HostedAccount[]
  pools: HostingPool[]
  summary: { standard_cost: number; hosting_cost: number; fallback_cost: number }
  two_fa_login_available?: boolean
  two_fa_login_host?: string
}
export interface HostingTwoFAJob { id: string; status: 'running' | 'succeeded' | 'failed' }
export interface HostingAdminAccount extends HostedAccount { user_id: number; user_email: string; username: string; pool_name: string }
export interface HostingLoginCredentials { email: string; password: string; mfa_secret: string }
export interface HostingPool { id: number; name: string; account_count: number; key_count: number }
export interface HostingSettings {
  enabled: boolean
  default_seats: number
  rate_multiplier: number
}
export const hostingAPI = {
  syncModels: async (id: number, admin = false) => (await apiClient.post<SyncUpstreamModelsResult>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/operations/sync-models`)).data,
  operation: async (id: number, operation: 'refresh' | 'recover-state' | 'set-privacy', admin = false) => (await apiClient.post<HostedAccount>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/operations/${operation}`, undefined, { timeout: 90_000 })).data,
  models: async (id: number, admin = false) => (await apiClient.get<ClaudeModel[]>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/operations/models`)).data,
  tempState: async (id: number, admin = false) => (await apiClient.get<TempUnschedulableStatus>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/operations/temp-unschedulable`)).data,
  accountStats: async (id: number, days = 30, admin = false) => (await apiClient.get<AccountUsageStatsResponse>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/stats`, { params: { days } })).data,
  billing: async (accountId = 0, route = '', page = 1, pageSize = 20, admin = false) => (await apiClient.get<HostingBilling>(`${admin ? '/admin' : ''}/hosting${accountId ? `/accounts/${accountId}` : ''}/billing`, { params: { route, page, page_size: pageSize } })).data,
  accountUsage: async (id: number, force = false, admin = false) => (await apiClient.get<AccountUsageInfo>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/usage`, { params: { force } })).data,
  refreshQuota: async (id: number, admin = false) => (await apiClient.post<OpenAIQuotaRefreshResult>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/quota/refresh`)).data,
  resetQuota: async (id: number, admin = false) => (await apiClient.post<Omit<OpenAIQuotaResetResult, 'account'> & { account?: HostedAccount | null }>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/reset-quota`, undefined, { timeout: 90_000 })).data,
  adminAccountPools: async (id: number) => (await apiClient.get<HostingPool[]>(`/admin/hosting/accounts/${id}/pools`)).data,
  overview: async () => (await apiClient.get<HostingOverview>('/hosting')).data,
  adminAccounts: async (page = 1, pageSize = 20, search = '') => (await apiClient.get<{ items: HostingAdminAccount[]; total: number; page: number; page_size: number }>('/admin/hosting/accounts', { params: { page, page_size: pageSize, search } })).data,
  startTwoFALogin: async (credentials: HostingLoginCredentials, replaceId = 0, poolId = 0) => (await apiClient.post<HostingTwoFAJob>('/hosting/two-fa-login', { ...credentials, replace_id: replaceId, hosting_pool_id: poolId })).data,
  getTwoFALogin: async (id: string) => (await apiClient.get<HostingTwoFAJob>(`/hosting/two-fa-login/${id}`)).data,
  deleteTwoFALogin: async (id: string) => apiClient.delete(`/hosting/two-fa-login/${id}`),
  completeTwoFALogin: async (id: string, settings: HostedAccountSettings) => (await apiClient.post<HostedAccount>(`/hosting/two-fa-login/${id}/complete`, { settings })).data,
  status: async () => (await apiClient.get<{ enabled: boolean; rate_multiplier: number; pools: HostingPool[] }>('/hosting/status')).data,
  import: async (content: string, name: string, replaceId = 0, poolId = 0, settings?: HostedAccountSettings) => (await apiClient.post<HostedAccount>('/hosting/import', { content, name, replace_id: replaceId, hosting_pool_id: poolId, settings })).data,
  startOAuth: async (replaceId = 0, poolId = 0) => (await apiClient.post<{ auth_url: string; session_id: string }>('/hosting/oauth/start', { replace_id: replaceId, hosting_pool_id: poolId })).data,
  savePool: async (name: string, id = 0) => (await (id ? apiClient.put<HostingPool>(`/hosting/pools/${id}`, { name }) : apiClient.post<HostingPool>('/hosting/pools', { name }))).data,
  deletePool: async (id: number) => apiClient.delete(`/hosting/pools/${id}`),
  completeOAuth: async (sessionId: string, callback: string, name: string, settings?: HostedAccountSettings) => {
    const url = new URL(callback.trim())
    const code = url.searchParams.get('code')
    const state = url.searchParams.get('state')
    if (!code || !state) throw new Error('Invalid OAuth callback URL')
    return (await apiClient.post<HostedAccount>('/hosting/oauth/complete', { session_id: sessionId, code, state, name, settings })).data
  },
  change: async (id: number, action: HostingAccountAction, admin = false) => action === 'delete'
    ? apiClient.delete(`${admin ? '/admin' : ''}/hosting/accounts/${id}`)
    : apiClient.post(`${admin ? '/admin' : ''}/hosting/accounts/${id}/${action}`),
  settings: async () => (await apiClient.get<HostingSettings>('/admin/hosting/settings')).data,
  saveAccountSettings: async (id: number, settings: HostedAccountSettings, admin = false) => (await apiClient.put<HostedAccount>(`${admin ? '/admin' : ''}/hosting/accounts/${id}/settings`, settings)).data,
  saveSettings: async (settings: HostingSettings) => apiClient.put('/admin/hosting/settings', settings),
  policy: async (id: number) => (await apiClient.get<{ policy: HostingPolicy; seat_limit: number; rate_multiplier: number }>(`/admin/hosting/users/${id}`)).data,
  savePolicy: async (id: number, policy: HostingPolicy) => apiClient.put(`/admin/hosting/users/${id}`, policy),
}
export function hostingFallbackGroups(groups: Group[]): Group[] {
  return groups.filter(group => group.platform === 'openai' && group.subscription_type !== 'subscription' && group.status === 'active')
}

export function hostingErrorMessage(error: unknown, fallback: string): string {
  if (error && typeof error === 'object' && 'message' in error && typeof error.message === 'string') return error.message
  return fallback
}
