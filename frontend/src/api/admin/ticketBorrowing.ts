import { apiClient } from '../client'

export interface BorrowSource {
  use_proxy_pool?: boolean; pool_entry_id?: number; pool_endpoint?: string; fingerprint_id?: string
  id: number; account_id: number; selected_group_ids: number[]; explicit: boolean; unavailable_reason: string; name: string; model: string; enabled: boolean; revision: number
  rotate_identity: boolean; proxy_count: number; last_reason: string; next_attempt_at: string
}
export interface BorrowSourceInput {
  name: string; model: string; enabled: boolean; revision?: number
  rotate_identity: boolean; credentials?: Record<string, unknown>; proxies?: string[]
}
export interface BorrowSelection {
  use_proxy_pool?: boolean
  revision: number; group_ids: number[]; account_ids: number[]; default_model: string
  rotate_identity: boolean; model_overrides: Record<number, string>
}
export interface BorrowSourceOption { id: number; name: string; group_ids: number[]; status: string; schedulable: boolean; is_target: boolean }
export interface BorrowSourceOptions { accounts: BorrowSourceOption[]; groups: {id: number; name: string}[]; legacy_count: number; restorable_count: number }
export interface BorrowProbePolicy { revision: number; concurrency: number; renew_before_seconds: number }
export interface BorrowProbeBatchAccount { account_id: number; revision: number; name: string }
export interface BorrowProbeBatchInput {
 accounts: { account_id: number; revision: number }[]
 concurrency?: number
 renew_before_seconds?: number
}
export interface BorrowProbeBatchResult { account_id: number; status: 'saved' | 'failed'; reason?: string }
export interface BorrowTarget {
  probe_policy?: BorrowProbePolicy
  upstream_wait_until?: string; upstream_wait_reason?: string; budget_wait_until?: string
  cooldown_until?: string; cooldown_seconds?: number; recovery_level?: number; recovery_epoch?: number
  consecutive_failures?: number; inflight?: number; probe_requests_last_minute?: number
  last_request_started_at?: string; last_request_finished_at?: string
  account_id: number; name: string; enabled: boolean; revision: number
  scan_completed?: number; scan_total?: number; refresh_requested?: boolean;
 ready: boolean; phase: string; reason: string; gateway?: string
  expires_at?: string; server_now: string; next_attempt_at: string
}
export interface BorrowRecoverySettings { enabled: boolean; failure_threshold: number; cooldown_seconds: number[]; probe_requests_per_minute: number; source_requests_per_minute?: number }
export interface BorrowSettings { revision?:number; running: boolean; ttl_seconds: number; prewarm_seconds: number; recovery?: BorrowRecoverySettings; server_now?: string; source_budget?: { used: number; limit: number; available_at?: string } }
export interface BorrowEnvironment {
  identity_hash?: string; profile_hash?: string; configured_proxy_hash?: string; client_version?: string; body_hash?: string
}
export interface BorrowCandidateDetails {
 rotation?: {job_id: string; role: string; trigger: string; account_id: number; previous_entry_id: number; entry_id: number; environment_version: number; selection_kind?: string}
  candidate_id: string; batch_id: string; source_account_id?: number; source_model?: string; source_format?: string
  source_revision?: number; target_revision?: number; source: BorrowEnvironment; target: BorrowEnvironment
  target_identity_hash?: string; cookie_hash?: string; gateway?: string
  cookie_observed_at?: string; expires_at?: string; cookie_age_ms?: number; remaining_ms?: number
  verification_prepared_at?: string; cookie_age_at_probe_ms?: number; remaining_at_probe_ms?: number
  source_attempts?: number; probe_retries?: number; probe_verdict?: string; probe_failure?: string; mint_status?: number; continue_status?: number
}
export interface BorrowEvent {
  id: number; target_id: number; source_id: number; source_account_id?: number; route_id: string
  stage: string; reason: string; created_at: string
  details?: BorrowCandidateDetails
}
const base = '/admin/accounts/ticket-borrowing'
export const ticketBorrowing = {
 rotations: async () => (await apiClient.get<BorrowRotationState[]>(`${base}/rotations`)).data,
 rotate: async (role: 'source', account_ids: number[], request_id: string) => (await apiClient.post<BorrowRotationJob>(`${base}/rotations`, { role, account_ids, request_id })).data,
 saveRotationPolicy: async (id: number, policy: BorrowRotationPolicy) => (await apiClient.put<BorrowRotationState[]>(`/admin/accounts/${id}/ticket-borrowing/rotation`, policy)).data,
 saveRotationPolicies: async (input: BorrowRotationBatchInput) => (await apiClient.put<BorrowRotationBatchResult[]>(`${base}/rotations`, input)).data,
  selection: async () => (await apiClient.get<BorrowSelection>(`${base}/selection`)).data,
  saveSelection: async (value: BorrowSelection) => (await apiClient.put<BorrowSelection>(`${base}/selection`, value)).data,
  sourceOptions: async () => (await apiClient.get<BorrowSourceOptions>(`${base}/source-options`)).data,
  restoreSources: async () => (await apiClient.post<{account_ids:number[];restored:number}>(`${base}/restore-sources`, {confirmed:true})).data,
  transferPreview: async (id: number) => (await apiClient.get<{account_id:number;name:string;model:string;has_proxy:boolean;schedulable:boolean}>(`${base}/transfer/${id}`)).data,
  scanTarget: async (id:number, cancel = false) => cancel ? apiClient.delete(`/admin/accounts/${id}/ticket-borrowing/scan`) : apiClient.post(`/admin/accounts/${id}/ticket-borrowing/scan`),
  transferSource: async (accountID: number) => (await apiClient.post<BorrowSource>(`${base}/transfer`, { account_id: accountID, confirmed: true })).data,
  sources: async () => (await apiClient.get<BorrowSource[]>(`${base}/sources`)).data,
  saveSource: async (input: BorrowSourceInput, id?: number) => id
    ? (await apiClient.put<BorrowSource>(`${base}/sources/${id}`, input)).data
    : (await apiClient.post<BorrowSource>(`${base}/sources`, input)).data,
  deleteSource: async (source: BorrowSource) => apiClient.delete(`${base}/sources/${source.id}`, { params: { revision: source.revision } }),
  targets: async () => (await apiClient.get<BorrowTarget[]>(`${base}/targets`)).data,
  saveProbePolicy: async (id: number, policy: BorrowProbePolicy) => (await apiClient.put<BorrowTarget>(`/admin/accounts/${id}/ticket-borrowing/probe-policy`, policy)).data,
  saveProbePolicies: async (input: BorrowProbeBatchInput) => (await apiClient.put<BorrowProbeBatchResult[]>(`${base}/probe-policies`, input)).data,
  target: async (id: number) => (await apiClient.get<BorrowTarget>(`/admin/accounts/${id}/ticket-borrowing`)).data,
  setTarget: async (id: number, enabled: boolean, revision: number) => (await apiClient.put<BorrowTarget>(`/admin/accounts/${id}/ticket-borrowing`, { enabled, revision })).data,
  refreshTarget: async (id: number) => apiClient.post(`/admin/accounts/${id}/ticket-borrowing/refresh`),
  settings: async () => (await apiClient.get<BorrowSettings>(`${base}/settings`)).data,
  saveSettings: async (value: BorrowSettings) => (await apiClient.put<BorrowSettings>(`${base}/settings`, value)).data,
  events: async (before = 0) => (await apiClient.get<BorrowEvent[]>(`${base}/events`, { params: { before } })).data
}

export interface BorrowRotationPolicy { revision: number; automatic: boolean; interval_seconds: number }
export interface BorrowRotationBatchAccount { account_id: number; revision: number; name: string }
export interface BorrowRotationBatchInput {
 accounts: { account_id: number; revision: number }[]
 automatic?: boolean
 interval_seconds?: number
}
export interface BorrowRotationBatchResult { account_id: number; status: 'saved' | 'failed'; reason?: string }
export interface BorrowRotationState extends BorrowRotationPolicy {
 account_id: number; role: 'source'; phase: string; reason: string; selection_kind?: 'random' | 'global_new' | 'source_new' | 'least_recent'; active_job?: string
 next_at?: string; last_at?: string; pool_entry_id?: number; pool_endpoint?: string; fingerprint_id?: string; environment_version: number; server_now: string
}
export interface BorrowRotationJob { id: string; role: 'source'; account_ids: number[]; phase: string; reason: string }
