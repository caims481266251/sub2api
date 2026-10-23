import { mount, flushPromises, enableAutoUnmount } from '@vue/test-utils'
import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import SavedTwoFAReauth from '../SavedTwoFAReauth.vue'
import { apiClient } from '@/api/client'
import { exportTOTP } from '@/api/admin/openaiTotp'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/client', () => ({ apiClient: { get: vi.fn(), post: vi.fn() } }))
vi.mock('@/api/admin/openaiTotp', () => ({ exportTOTP: vi.fn() }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: async (text: string) => { await navigator.clipboard.writeText(text); return true } }) }))
enableAutoUnmount(afterEach)
const config = { configured: true, credential_mode: 'password_totp', totp_configured: true }
beforeEach(() => {
 vi.resetAllMocks()
 vi.mocked(apiClient.get).mockResolvedValue({ data: { config } })
 Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockResolvedValue(undefined) } })
})
it('loads only non-secret status until an explicit copy, then copies the complete format', async () => {
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } })
 await flushPromises()
 expect(exportTOTP).not.toHaveBeenCalled()
 const secret = { email: 'user@example.com', password: 'test-password', secret: 'TESTSECRET', otpauth_uri: 'otpauth://test', state: 'saved', source: 'current' as const }
 vi.mocked(exportTOTP).mockResolvedValue(secret)
 await wrapper.get('[data-testid="saved-two-fa-copy"]').trigger('click'); await flushPromises()
 expect(exportTOTP).toHaveBeenCalledWith(42, 'current', true, expect.any(AbortSignal))
 expect(navigator.clipboard.writeText).toHaveBeenCalledWith('user@example.com----test-password----TESTSECRET')
 expect(wrapper.text()).not.toContain('test-password'); expect(wrapper.text()).not.toContain('TESTSECRET')
 expect(secret.password).toBe(''); expect(secret.secret).toBe('')
})
it('uses saved credentials without sending secrets and merges repeated button clicks', async () => {
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } }); await flushPromises()
 const task = { id: 9, status: 'queued', account_id: 42 }
 vi.mocked(apiClient.post).mockResolvedValue({ data: task })
 vi.mocked(apiClient.get).mockResolvedValue({ data: { config, task } })
 await wrapper.get('[data-testid="saved-two-fa-reauth"]').trigger('click'); await flushPromises()
 await wrapper.get('[data-testid="saved-two-fa-reauth"]').trigger('click'); await flushPromises()
 expect(apiClient.post).toHaveBeenCalledTimes(1)
 expect(apiClient.post).toHaveBeenCalledWith('/admin/accounts/42/openai-reauth', { oauth_profile: 'codex' }, expect.objectContaining({ signal: expect.any(AbortSignal) }))
 expect(exportTOTP).not.toHaveBeenCalled()
})
it('does not fetch or copy missing credentials', async () => {
 vi.mocked(apiClient.get).mockResolvedValue({ data: {} })
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } }); await flushPromises()
 expect(wrapper.get('[data-testid="saved-two-fa-copy"]').attributes('disabled')).toBeDefined()
 expect(wrapper.get('[data-testid="saved-two-fa-reauth"]').attributes('disabled')).toBeDefined()
 expect(exportTOTP).not.toHaveBeenCalled()
})
it('does not copy a late secret response after unmount', async () => {
 let finish!: (v: Awaited<ReturnType<typeof exportTOTP>>) => void
 vi.mocked(exportTOTP).mockReturnValue(new Promise(resolve => { finish = resolve }))
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } }); await flushPromises()
 await wrapper.get('[data-testid="saved-two-fa-copy"]').trigger('click'); wrapper.unmount()
 const credential = { email: 'user@example.com', password: 'private', secret: 'PRIVATE', otpauth_uri: '', state: 'saved', source: 'current' as const }
 finish(credential); await flushPromises()
 expect(navigator.clipboard.writeText).not.toHaveBeenCalled(); expect(credential.password).toBe(''); expect(credential.secret).toBe('')
})
it('does not echo server diagnostics on errors', async () => {
 vi.mocked(apiClient.get).mockRejectedValue(new Error('secret-provider-response'))
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } }); await flushPromises()
 expect(wrapper.text()).toContain('savedTwoFA.loadFailed'); expect(wrapper.text()).not.toContain('secret-provider-response')
})

it('does not treat an old successful task as a new reauthorization', async () => {
 vi.mocked(apiClient.get).mockResolvedValue({ data: { config, task: { id: 8, status: 'succeeded' } } })
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } }); await flushPromises()
 expect(wrapper.emitted('reauthorized')).toBeUndefined()
 expect(wrapper.get('a').attributes('href')).toBe('/admin/token-guard-v2')
})
it('announces completion only for the observed recovery task', async () => {
 const wrapper = mount(SavedTwoFAReauth, { props: { accountId: 42 } }); await flushPromises()
 vi.mocked(apiClient.post).mockResolvedValue({ data: { id: 10, status: 'queued' } })
 vi.mocked(apiClient.get).mockResolvedValue({ data: { config, task: { id: 10, status: 'succeeded' } } })
 await wrapper.get('[data-testid="saved-two-fa-reauth"]').trigger('click'); await flushPromises()
 expect(wrapper.emitted('reauthorized')).toHaveLength(1)
})
