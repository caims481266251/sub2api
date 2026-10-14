import type { Account } from '@/types'
import type { OpenAIQuotaRefreshResult, OpenAIQuotaResetResult } from '@/api/admin/accounts'

// The quota UI needs display data only, never OAuth credentials.
export type OpenAIQuotaAccount = Pick<Account, 'id' | 'name' | 'platform' | 'type' | 'extra' | 'parent_account_id'>

export interface OpenAIQuotaTransport {
  refresh: (id: number) => Promise<OpenAIQuotaRefreshResult>
  reset: (id: number) => Promise<OpenAIQuotaResetResult>
}
