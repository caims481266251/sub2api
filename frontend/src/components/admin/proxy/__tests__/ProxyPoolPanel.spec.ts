import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ProxyPoolPanel from '../ProxyPoolPanel.vue'

const api = vi.hoisted(() => ({ state: vi.fn(), preview: vi.fn(), import: vi.fn(), initializeEncryption: vi.fn(), setEnabled: vi.fn() }))
vi.mock('@/api/admin/proxyPool', () => ({ proxyPool: api }))
vi.mock('vue-router', () => ({ onBeforeRouteLeave: vi.fn() }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : ''), te: () => true }) }))
const entry = { id: 1, protocol: 'http', host: 'example.com', port: 80, enabled: true, authenticated: true, created_at: new Date().toISOString() }
const state = () => ({ revision: 3, entries: [entry], encryption_configured: true })
const preview = () => ({ revision: 3, mode: 'append', valid_count: 1, duplicate_count: 0, added_count: 1, retained_count: 0, removed_count: 0, issues: [], entries: [], can_apply: true })
let w: VueWrapper
beforeEach(() => { vi.clearAllMocks(); api.state.mockResolvedValue(state()); api.preview.mockResolvedValue(preview()); api.import.mockResolvedValue({ ...state(), revision: 4 }); api.setEnabled.mockResolvedValue({ ...state(), revision: 4, entries: [{ ...entry, enabled: false }] }) })
afterEach(() => w?.unmount())
async function ready() { w = mount(ProxyPoolPanel); await flushPromises() }
async function previewDraft() { await w.get('[data-testid="pool-input"]').setValue('example.com:80:user:private-password'); await w.get('form').trigger('submit'); await flushPromises() }
describe('ordinary proxy inventory', () => {
  it('previews before applying and clears plaintext after a successful import', async () => {
    await ready(); await previewDraft()
    expect(api.import).not.toHaveBeenCalled()
    expect(w.get('[data-testid="pool-preview-result"]').text()).not.toContain('private-password')
    await w.get('[data-testid="pool-apply"]').trigger('click'); await flushPromises()
    expect(api.import).toHaveBeenCalledWith({ revision: 3, mode: 'append', text: 'example.com:80:user:private-password' })
    expect((w.get('textarea').element as HTMLTextAreaElement).value).toBe('')
    expect(w.find('[data-testid="pool-preview-result"]').exists()).toBe(false)
  })
  it('invalidates preview after changing input or replacement mode', async () => {
    await ready(); await previewDraft()
    await w.get('[data-testid="pool-mode"]').setValue('replace')
    expect(w.find('[data-testid="pool-apply"]').exists()).toBe(false)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(api.preview).toHaveBeenLastCalledWith(expect.objectContaining({ mode: 'replace' }))
    await w.get('textarea').setValue('changed.example:80')
    expect(w.find('[data-testid="pool-apply"]').exists()).toBe(false)
  })
  it('blocks all writes when preview contains an invalid line', async () => {
    api.preview.mockResolvedValue({ ...preview(), issues: [{ line: 2, reason: 'invalid_proxy' }], can_apply: false })
    await ready(); await previewDraft()
    expect(w.get('[data-testid="pool-apply"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('proxyPool.issues.invalid_proxy')
    expect(api.import).not.toHaveBeenCalled()
  })
  it('retains a draft after conflict and requires a new preview', async () => {
    api.import.mockRejectedValue({ reason: 'proxy_pool_conflict' })
    await ready(); await previewDraft(); await w.get('[data-testid="pool-apply"]').trigger('click'); await flushPromises()
    expect(w.get('[role="alert"]').text()).toContain('proxy_pool_conflict')
    expect((w.get('textarea').element as HTMLTextAreaElement).value).toContain('private-password')
    expect(w.find('[data-testid="pool-apply"]').exists()).toBe(false)
    expect(api.import).toHaveBeenCalledOnce()
  })
  it('toggles entries using a revision and protects unsaved input on close', async () => {
    await ready(); await previewDraft()
    const event = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(event); expect(event.defaultPrevented).toBe(true)
    await w.get('[data-pool-entry="1"] button').trigger('click'); await flushPromises()
    expect(api.setEnabled).toHaveBeenCalledWith(1, 3, false)
    expect(w.get('[data-pool-entry="1"]').text()).toContain('proxyPool.disabled')
    expect(w.find('[data-testid="pool-apply"]').exists()).toBe(false)
  })
})
