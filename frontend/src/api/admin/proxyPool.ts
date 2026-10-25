import { apiClient } from '../client'

export interface ProxyPoolEntry {
  id: number
  protocol: string
  host: string
  port: number
  authenticated: boolean
  enabled: boolean
  created_at: string
}
export interface ProxyPoolState {
  revision: number
  entries: ProxyPoolEntry[]
  encryption_configured: boolean
}
export interface ProxyPoolInput { revision: number; mode: 'append' | 'replace'; text: string }
export interface ProxyPoolPreview {
  revision: number
  mode: 'append' | 'replace'
  valid_count: number
  duplicate_count: number
  added_count: number
  retained_count: number
  removed_count: number
  issues: { line: number; reason: string }[]
  entries: ProxyPoolEntry[]
  can_apply: boolean
}
export const proxyPool = {
  state: async (): Promise<ProxyPoolState> => (await apiClient.get('/admin/proxies/pool')).data,
  preview: async (input: ProxyPoolInput): Promise<ProxyPoolPreview> => (await apiClient.post('/admin/proxies/pool/preview', input)).data,
  import: async (input: ProxyPoolInput): Promise<ProxyPoolState> => (await apiClient.put('/admin/proxies/pool', input)).data,
  initializeEncryption: async (): Promise<ProxyPoolState> => (await apiClient.post('/admin/proxies/pool/encryption')).data,
  setEnabled: async (id: number, revision: number, enabled: boolean): Promise<ProxyPoolState> => (await apiClient.put(`/admin/proxies/pool/entries/${id}`, { revision, enabled })).data
}
