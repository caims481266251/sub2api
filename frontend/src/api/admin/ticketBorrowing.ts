import { apiClient } from '../client'

export interface BorrowSource {
  can_rotate?: boolean; rotation_unavailable_reason?: string
  rate_limit_reset_at?: string
  use_proxy_pool?: boolean; pool_entry_id?: number; pool_endpoint?: string; fingerprint_id?: string
  id: number; account_id: number; selected_group_ids: number[]; explicit: boolean; unavailable_reason: string; name: string; model: string; enabled: boolean; revision: number
  rotate_identity: boolean; proxy_count: number; last_reason: string; next_attempt_at: string
}
export interface BorrowSourceInput {
  name: string; model: string; enabled: boolean; revision?: number
  rotate_identity: boolean; credentials?: Record<string, unknown>; proxies?: string[]
}
export interface BorrowSelection {
  excluded_account_ids?: number[]
  use_proxy_pool?: boolean
  revision: number; group_ids: number[]; account_ids: number[]; default_model: string
  rotate_identity: boolean; model_overrides: Record<number, string>
}
export interface BorrowSourceOption { request_eligible?: boolean; unavailable_reason?: string; auth_excluded?: boolean; id: number; name: string; group_ids: number[]; status: string; schedulable: boolean; is_target: boolean }
export interface BorrowSourceOptions { accounts: BorrowSourceOption[]; groups: {id: number; name: string}[]; legacy_count: number; restorable_count: number }
export interface BorrowProbePolicy { revision: number; concurrency: number; renew_before_seconds: number; use_global?: boolean; global_concurrency?: number; global_renew_before_seconds?: number }
export interface BorrowProbeBatchAccount { account_id: number; revision: number; name: string }
export interface BorrowProbeBatchInput {
 accounts: { account_id: number; revision: number }[]
 use_global?: boolean
 concurrency?: number
}
export interface BorrowProbeBatchResult { account_id: number; status: 'saved' | 'failed'; reason?: string }
export interface BorrowTarget {
 acquisition_identity?: 'self' | 'source'; acquisition_revision?: number
 stored_inventory_count?: number; inventory_count?: number; desired_inventory?: number; maintenance_state?: 'search' | 'refill' | 'renew' | 'healthy'; next_probe_at?: string; earliest_admit_until?: string; refresh_generation?: number; blocked_reason?: string

 preferred_route_id?: string
 business_routes?: BorrowRouteStatus[]
 business_inflight?: number
 business_concurrency?: number
  probe_policy?: BorrowProbePolicy
  upstream_wait_until?: string; upstream_wait_reason?: string; budget_wait_until?: string
  cooldown_until?: string; cooldown_seconds?: number; recovery_level?: number; recovery_epoch?: number
  consecutive_failures?: number; inflight?: number; probe_requests_last_minute?: number
  last_request_started_at?: string; last_request_finished_at?: string
  account_id: number; name: string; enabled: boolean; revision: number
  scan_completed?: number; scan_total?: number; refresh_requested?: boolean;
 ready: boolean; phase: string; reason: string; gateway?: string
  expires_at?: string; usable_until?: string; server_now: string; next_attempt_at: string
}
export interface BorrowRouteQualityConfig { model: string; reasoning_effort: string; interval_seconds: number }
export interface BorrowRouteQualityResult { status: string; model: string; reasoning_effort: string; answer?: string; reason?: string; http_status: number; checked_at: string; duration_ms: number }
export interface BorrowRouteStatus {
 // Remaining account capacity for an eligible route; shared, not additive.
 available_slots?: number; unavailable_reason?: string
 quality?: BorrowRouteQualityResult; quality_valid_until?: string
 state?: 'normal' | 'pending_quality' | 'pending_probe' | 'cooling' | 'expired' | 'invalid'; generation?: number
 cooldown_until?: string; last_success_at?: string; last_business_success_at?: string; last_error_at?: string; last_error?: string
 checking?: boolean; verification_model?: string; deadline_source?: string
 id: string; gateway: string; source_account_id: number; published_at?: string; checked_at?: string
 expires_at: string; usable_until: string
 inventory_eligible: boolean; usable: boolean; inflight: number
}
export interface BorrowRecoverySettings { enabled: boolean; failure_threshold: number; cooldown_seconds: number[]; probe_requests_per_minute: number; source_requests_per_minute?: number }
export interface BorrowSettings { route_quality?: BorrowRouteQualityConfig; route_pool_size?: number; route_cooldown_seconds?: number; continue_timeout_seconds?: number; revision?:number; running: boolean; probe_concurrency?: number; recovery?: BorrowRecoverySettings; server_now?: string; source_budget?: { used: number; limit: number; available_at?: string } }
export interface BorrowEnvironment {
  identity_hash?: string; profile_hash?: string; configured_proxy_hash?: string; client_version?: string; body_hash?: string
}
export interface BorrowHTTPAttempt {
 phase: string; number: number; started_at: string; duration_ms: number; status: number
 events: Record<string, number>; error_class?: string; failure_stage?: string; timeout_stage?: string; cancellation_cause?: string; terminal?: string; connection_reused?: boolean
}
export interface BorrowProbePhase { duration_ms: number; retry_count: number; attempts: BorrowHTTPAttempt[] }
export interface BorrowPhaseStatistics { requests: number; diagnostic_samples: number; retries: number; average_ms: number | null; timeouts: number; error_classes?: Record<string, number>; timeout_stages?: Record<string, number>; cancellations?: Record<string, number> }
export interface BorrowCandidateDetails {
 pool_enqueued_at?: string; pool_claimed_at?: string; pool_minimum_remaining_ms?: number
 source_http?: BorrowHTTPAttempt; mint?: BorrowProbePhase; continue?: BorrowProbePhase; cancellation_cause?: string; probe_queue_ms?: number
 gateway_priority?: string
 source_queue_ms?: number; source_preparation_ms?: number; source_network_ms?: number; publication_ms?: number
 acquisition_identity?: 'self' | 'source'; acquisition_started_at?: string; admission_until?: string; upstream_expires_at?: string
 pool_entry_id?: number; allocation_round?: number; proxy_session_randomized?: boolean; business?: BorrowEnvironment
 replenishment?: { environment_version?: number; until?: string; shadow?: boolean; inventory?: number; exit_reason?: string }
 source_environment_version?: number; source_rotation_revision?: number
 rotation?: {skipped?: boolean; job_id: string; role: string; trigger: string; account_id: number; previous_entry_id: number; entry_id: number; environment_version: number; selection_kind?: string}
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
export interface BorrowEventQuery { before?: number; after?: number; target_id?: number; source_account_id?: number; source_id?: number; stage?: string; reason?: string; candidate_id?: string; route_id?: string; search?: string; since?: string; until?: string; hide_rotations?: boolean }
const base = '/admin/accounts/ticket-borrowing'

export interface BorrowCandidatePoolConfig { capacity: number; revision: number }
export interface BorrowCandidatePoolSummary extends BorrowCandidatePoolConfig {
  running: boolean; occupied: number; acquiring: number; queued: number; claimed: number; waiting_targets: number; demand_slots: number; production_state?: 'filling' | 'paused' | 'full' | 'no_targets'
}
export type BorrowCandidatePoolState = 'acquiring' | 'queued' | 'claimed' | 'finished'
export interface BorrowCandidatePoolQuery {
  state?: BorrowCandidatePoolState | 'active' | 'all'; source_account_id?: number; target_id?: number; search?: string; page?: number; page_size?: number
}
export interface BorrowCandidatePoolItem {
  id: string; state: BorrowCandidatePoolState; source_id: number; source_account_id: number; source_name: string
  target_id: number; target_name: string; gateway: string; created_at: string; enqueued_at?: string; claimed_at?: string
  finished_at?: string; admission_until?: string; cookie_expires_at?: string; gateway_priority?: string; reason: string; route_id?: string
}
export interface BorrowCandidatePoolPage {
  summary: BorrowCandidatePoolSummary; items: BorrowCandidatePoolItem[]; total: number; page: number; page_size: number; server_now: string
}
export interface BorrowCandidatePoolDetail extends BorrowCandidatePoolItem { details?: BorrowCandidateDetails; server_now: string }

export const ticketBorrowing = {
 candidatePool: async (query: BorrowCandidatePoolQuery = {}) => (await apiClient.get<BorrowCandidatePoolPage>(`${base}/candidate-pool`, { params: query })).data,
 candidatePoolDetail: async (id: string) => (await apiClient.get<BorrowCandidatePoolDetail>(`${base}/candidate-pool/${encodeURIComponent(id)}`)).data,
 saveCandidatePoolConfig: async (value: BorrowCandidatePoolConfig) => (await apiClient.put<BorrowCandidatePoolConfig>(`${base}/candidate-pool/config`, value)).data,
 rotations: async () => (await apiClient.get<BorrowRotationState[]>(`${base}/rotations`)).data,
 rotate: async (role: 'source', account_ids: number[], request_id: string) => (await apiClient.post<BorrowRotationJob>(`${base}/rotations`, { role, account_ids, request_id })).data,
 saveRotationPolicy: async (id: number, policy: BorrowRotationPolicy) => (await apiClient.put<BorrowRotationState[]>(`/admin/accounts/${id}/ticket-borrowing/rotation`, policy)).data,
 saveRotationPolicies: async (input: BorrowRotationBatchInput) => (await apiClient.put<BorrowRotationBatchResult[]>(`${base}/rotations`, input)).data,
  statistics: async (target_id: number, minutes = 15) => (await apiClient.get<BorrowStatistics>(`${base}/statistics`, { params: { target_id, minutes } })).data,
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
  targets: async () => (await apiClient.get<BorrowTarget[]>(`${base}/targets`, { params: { include_disabled: true } })).data,
  saveProbePolicy: async (id: number, policy: BorrowProbePolicy) => (await apiClient.put<BorrowTarget>(`/admin/accounts/${id}/ticket-borrowing/probe-policy`, { revision: policy.revision, concurrency: policy.concurrency, use_global: policy.use_global })).data,
  saveProbePolicies: async (input: BorrowProbeBatchInput) => (await apiClient.put<BorrowProbeBatchResult[]>(`${base}/probe-policies`, input)).data,
  target: async (id: number) => (await apiClient.get<BorrowTarget>(`/admin/accounts/${id}/ticket-borrowing`)).data,
  setTarget: async (id: number, enabled: boolean, revision: number, acquisition_identity?: 'self' | 'source', acquisition_revision?: number) => (await apiClient.put<BorrowTarget>(`/admin/accounts/${id}/ticket-borrowing`, { enabled, revision, acquisition_identity, acquisition_revision })).data,
  refreshTarget: async (id: number) => apiClient.post(`/admin/accounts/${id}/ticket-borrowing/refresh`),
  settings: async () => (await apiClient.get<BorrowSettings>(`${base}/settings`)).data,
  saveControls: async (value: { revision: number; running?: boolean }) => (await apiClient.patch<BorrowSettings>(`${base}/settings`, value)).data,
  saveSettings: async (value: BorrowSettings) => (await apiClient.put<BorrowSettings>(`${base}/settings`, {
    revision: value.revision, running: value.running,
    route_quality: value.route_quality, route_pool_size: value.route_pool_size, route_cooldown_seconds: value.route_cooldown_seconds,
    probe_concurrency: value.probe_concurrency, continue_timeout_seconds: value.continue_timeout_seconds,
    recovery: value.recovery
  })).data,
  events: async (query: BorrowEventQuery | number = {}) => (await apiClient.get<BorrowEvent[]>(`${base}/events`, { params: typeof query === 'number' ? { before: query } : query })).data
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
 account_id: number; role: 'source'; phase: string; reason: string; selection_kind?: 'balanced_shared' | 'random' | 'global_new' | 'source_new' | 'least_recent'; active_job?: string
 next_at?: string; last_at?: string; pool_entry_id?: number; pool_endpoint?: string; fingerprint_id?: string; environment_version: number; server_now: string
}
export interface BorrowRotationJob { skipped?: { account_id: number; reason: string }[]; id: string; role: 'source'; account_ids: number[]; phase: string; reason: string }

export interface BorrowStatistics {
 mint?: BorrowPhaseStatistics; continue?: BorrowPhaseStatistics
 shared_target_inflight: number; shared_target_count: number
 candidates: number; gateways: number; source_failed: number; inconclusive: number; route_changed: number; prioritized: number; rechecked: number; last_success_at?: string
 target_id: number; since: string; until: string
 passed: number; published: number; ticket_changed: number; timeout: number; cancelled: number; other: number; skipped: number; inflight: number
 routes: { route_id: string; gateway: string; requests: number; completed: number; interrupted: number; unconfirmed: number; visible_samples: number; visible_average_ms: number | null; semantic_samples: number; semantic_average_ms: number | null; average_age_ms: number | null; latest_usage_id: number }[]
}
