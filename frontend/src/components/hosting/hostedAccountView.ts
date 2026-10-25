import type { Account } from '@/types'
import type { HostedAccount } from '@/api/hosting'

// The API sends an explicit runtime allowlist, never an admin credential payload.
export function hostedAccountView(a: HostedAccount): Account {
  const status = a.status ?? (a.state === 'login_required' ? 'error' : a.state === 'paused' ? 'inactive' : 'active')
  return {
    id: a.id, name: a.name, platform: 'openai', type: 'oauth',
    status: status === 'active' ? 'active' : status === 'error' ? 'error' : 'inactive',
    schedulable: a.schedulable ?? a.state !== 'paused',
    error_message: a.error_message ?? null,
    rate_limit_reset_at: a.rate_limit_reset_at ?? (a.state === 'quota_exhausted' ? a.reset_at : null),
    overload_until: a.overload_until ?? null,
    temp_unschedulable_until: a.temp_unschedulable_until ?? null,
    credentials: { ...a.settings.credentials, plan_type: a.plan_type },
    extra: { ...a.settings.extra, ...a.runtime_extra },
    base_rpm: Number(a.settings.extra.base_rpm) || 0,
    current_rpm: a.current_rpm ?? undefined, current_concurrency: a.current_concurrency ?? undefined,
    concurrency: a.settings.concurrency, priority: a.settings.priority,
    expires_at: a.settings.expires_at ?? null, auto_pause_on_expired: a.settings.auto_pause_on_expired ?? true,
    proxy_id: null, last_used_at: a.last_used_at, created_at: a.created_at ?? '', updated_at: '',
    rate_limited_at: null, temp_unschedulable_reason: null,
    session_window_start: null, session_window_end: null, session_window_status: null,
  }
}
