import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import zh from '@/i18n/locales/zh'
import type { HostedAccount, HostingOverview } from '@/api/hosting'
import Pagination from '@/components/common/Pagination.vue'
import HostingView from '../HostingView.vue'

const mocks = vi.hoisted(() => ({
  accountUsage: vi.fn(), change: vi.fn(), overview: vi.fn(), import: vi.fn(), startOAuth: vi.fn(), completeOAuth: vi.fn(), saveAccountSettings: vi.fn(),
  showSuccess: vi.fn(), showError: vi.fn(),
}))
vi.mock('@/api/hosting', async importOriginal => ({ ...await importOriginal<typeof import('@/api/hosting')>(), hostingAPI: mocks }))
vi.mock('@/components/common/Select.vue', () => ({ default: {
  props: ['modelValue', 'options', 'disabled'], emits: ['update:modelValue'],
  template: `<select :value="modelValue" :disabled="disabled" @change="$emit('update:modelValue', Number.isFinite(Number($event.target.value)) ? Number($event.target.value) : $event.target.value)"><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>`,
} }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.showSuccess, showError: mocks.showError, cachedPublicSettings: { excel_bps_enabled: true, prism_browser_enabled: true } }) }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => {
    let value: unknown = zh
    for (const part of key.split('.')) value = value && typeof value === 'object' ? (value as Record<string, unknown>)[part] : undefined
    if (typeof value !== 'string') throw new Error(`Missing translation: ${key}`)
    return value
  } }),
}))
const hosted = (): HostedAccount => ({ id: 17, name: '原账号', email: 'user@example.com', state: 'active', reset_at: null, last_used_at: null, usage_5h_percent: null, usage_7d_percent: null, settings: { hosting_pool_id: 31, name: '原账号', concurrency: 6, priority: 20, notes: '保留备注', credentials: { model_mapping_mode: 'aliases' }, extra: { openai_compact_mode: 'force_on' } } })
const overview = (): HostingOverview => ({ enabled: true, seat_limit: 3, used_seats: 1, rate_multiplier: .03, pools: [{ id: 0, name: '', account_count: 0, key_count: 0 }, { id: 31, name: '工作池', account_count: 1, key_count: 0 }], accounts: [hosted()], two_fa_login_available: true, two_fa_login_host: 'login.example.test', summary: { standard_cost: 0, hosting_cost: 0, fallback_cost: 0 } })
let wrapper: ReturnType<typeof mount>
const button = (text: string) => wrapper.findAll('button').find(item => item.text() === text)!
async function render() {
  wrapper = mount(HostingView, { global: { stubs: {
    AppLayout: { template: '<div><slot/></div>' },
    BaseDialog: { props: ['show', 'title'], template: '<div v-if="show" data-testid="hosting-dialog"><h2>{{ title }}</h2><slot/><slot name="footer"/></div>' },
    Teleport: true, ConfirmDialog: true, RouterLink: true, ModelWhitelistSelector: true,
  } } })
  await flushPromises()
}
async function configure() {
  await wrapper.get('[data-testid="hosted-name"]').setValue('工作 Codex')
  await wrapper.get(wrapper.find('#hosting-import-form').exists() ? '#hosting-import-pool' : '[data-testid="hosted-pool"]').setValue(31)
  await wrapper.get('[data-testid="hosted-notes"]').setValue('创建时配置')
  await wrapper.get('[data-testid="hosted-load-factor"]').setValue(8)
  await wrapper.get('[data-testid="hosted-openai_compact_mode"]').setValue('force_on')
  if (wrapper.find('#hosting-add-form').exists()) await wrapper.get('#hosting-add-form').trigger('submit')
  await flushPromises()
}
async function uploadAuthFile() {
  await wrapper.get('[data-testid="hosting-import-files"]').trigger('click')
  const file = new File(['{"tokens":{"refresh_token":"test-only"}}'], 'auth.json', { type: 'application/json' })
  Object.defineProperty(file, 'text', { value: async () => '{"tokens":{"refresh_token":"test-only"}}' })
  Object.defineProperty(wrapper.get('#hosting-file').element, 'files', { value: [file] })
  await wrapper.get('#hosting-file').trigger('change'); await flushPromises()
}
async function submitImport() {
  if (wrapper.find('#hosting-import-form').exists()) await wrapper.get('#hosting-import-form').trigger('submit')
  else await wrapper.get('[data-testid="hosting-import-submit"]').trigger('click')
  await flushPromises()
}
beforeEach(() => {
  Object.values(mocks).forEach(mock => mock.mockReset())
  mocks.overview.mockResolvedValue(overview())
  mocks.import.mockResolvedValue(hosted())
  mocks.completeOAuth.mockResolvedValue(hosted())
  mocks.startOAuth.mockResolvedValue({ session_id: 'session', auth_url: 'https://example.com/authorize' })
})
afterEach(() => wrapper?.unmount())

describe('Hosting add account flow', () => {
  it('shows the shared account configuration before authorization and submits it in one OAuth creation request', async () => {
    await render(); await button('添加账号').trigger('click')
    expect(wrapper.get('[data-testid="hosting-add-configuration"]').isVisible()).toBe(true)
    expect(wrapper.find('[data-testid="hosting-method-file"]').exists()).toBe(false)
    expect(wrapper.find('#hosting-callback').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Prism')
    expect(wrapper.find('[data-testid="hosted-bps-network"]').exists()).toBe(false)
    await configure()
    expect(wrapper.get('[data-testid="hosting-add-configuration"]').isVisible()).toBe(false)
    expect(wrapper.get('[data-testid="hosting-add-authorization"]').text()).toContain('工作 Codex · 工作池')
    await button('生成授权链接').trigger('click'); await flushPromises()
    expect(mocks.startOAuth).toHaveBeenCalledWith(0, 31)
    await wrapper.get('#hosting-callback').setValue('http://localhost:1455/auth/callback?code=code&state=state')
    await wrapper.get('#hosting-add-form').trigger('submit'); await flushPromises()
    expect(mocks.completeOAuth).toHaveBeenCalledWith('session', expect.any(String), '工作 Codex', expect.objectContaining({ name: '工作 Codex', hosting_pool_id: 31, notes: '创建时配置', load_factor: 8, extra: expect.objectContaining({ openai_compact_mode: 'force_on' }) }))
    expect(mocks.saveAccountSettings).not.toHaveBeenCalled()
    expect(wrapper.find('#hosting-add-form').exists()).toBe(false)
    expect(wrapper.find('#hosting-settings-form').exists()).toBe(false)
  })

  it('opens file import with a pool selector and default settings', async () => {
    await render(); await button('导入账号').trigger('click')
    expect(wrapper.get('[data-testid="hosting-dialog"] h2').text()).toBe('导入账号')
    expect(wrapper.find('#hosting-add-form').exists()).toBe(false)
    expect(wrapper.find('#hosting-file').exists()).toBe(true)
    expect(wrapper.find('#hosting-callback').exists()).toBe(false)
    expect(wrapper.get('[data-testid="hosting-import-submit"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="hosted-name"]').exists()).toBe(false)
    await wrapper.get('#hosting-import-pool').setValue(31)
    await uploadAuthFile()
    expect(wrapper.get('[data-testid="hosting-import-submit"]').attributes('disabled')).toBeUndefined()
    await submitImport()
    expect(mocks.import).toHaveBeenCalledWith(expect.any(String), 'Codex', 0, 31, expect.objectContaining({ concurrency: 3, priority: 50, extra: {} }))
    expect(mocks.saveAccountSettings).not.toHaveBeenCalled()
    expect(mocks.startOAuth).not.toHaveBeenCalled()
    expect(mocks.showSuccess).toHaveBeenCalledWith('导入成功')
    expect(wrapper.get('[data-testid="hosting-import-result"]').text()).toContain('导入成功')
    await button('取消').trigger('click')
    await button('添加账号').trigger('click')
    expect(wrapper.get('[data-testid="hosting-method-oauth"]').attributes('aria-pressed')).toBe('true')
  })

  it('imports an admin export immediately with its name and default hosted settings', async () => {
    await render(); await button('导入账号').trigger('click')
    await wrapper.get('[data-testid="hosting-import-paste"]').trigger('click')
    await wrapper.get('#hosting-import-json').setValue(JSON.stringify({ type: 'sub2api-data', version: 1, accounts: [{ name: 'Exported Codex', platform: 'openai', type: 'oauth', credentials: { refresh_token: 'export-refresh' }, proxy_key: 'admin-proxy', rate_multiplier: 7 }], proxies: [] }))
    await submitImport()
    expect(mocks.import).toHaveBeenCalledWith('{"refresh_token":"export-refresh"}', 'Exported Codex', 0, 0, expect.objectContaining({ name: 'Exported Codex', concurrency: 3, priority: 50, hosting_pool_id: 0 }))
    expect(mocks.import.mock.calls[0][4]).not.toHaveProperty('proxy_key')
    expect(mocks.import.mock.calls[0][4]).not.toHaveProperty('rate_multiplier')
    expect(mocks.startOAuth).not.toHaveBeenCalled()
  })

  it('does not advance or generate an OAuth session with invalid account configuration', async () => {
    await render(); await button('添加账号').trigger('click')
    await wrapper.get('[data-testid="hosted-error-rules-enabled"]').trigger('click')
    await wrapper.get('#hosting-add-form').trigger('submit')
    expect(wrapper.get('[data-testid="hosting-add-configuration"]').isVisible()).toBe(true)
    expect(wrapper.find('[data-testid="hosting-add-authorization"]').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('规则')
    expect(mocks.startOAuth).not.toHaveBeenCalled()
    expect(mocks.import).not.toHaveBeenCalled()
  })

  it('offers 2FA using the same configuration step without admin settings', async () => {
    await render(); await button('添加账号').trigger('click')
    await wrapper.get('[data-testid="hosting-method-two_fa"]').trigger('click')
    await configure()
    expect(wrapper.find('#hosting-login-password').exists()).toBe(true)
    expect(wrapper.find('#hosting-file').exists()).toBe(false)
    expect(wrapper.find('#hosting-callback').exists()).toBe(false)
    expect(wrapper.find('button[form="hosting-add-form"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('/admin/token-guard')
    expect(mocks.startOAuth).not.toHaveBeenCalled()
  })
  it('prefills reauthorization, locks its pool and allows importing credentials into the same account', async () => {
    await render(); await button('更多').trigger('click'); expect(button('智商测试')).toBeUndefined(); await button('重新授权').trigger('click')
    expect(wrapper.get<HTMLInputElement>('[data-testid="hosted-name"]').element.value).toBe('原账号')
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="hosted-notes"]').element.value).toBe('保留备注')
    expect(wrapper.get<HTMLSelectElement>('[data-testid="hosted-pool"]').element.disabled).toBe(true)
    expect(wrapper.get<HTMLSelectElement>('[data-testid="hosted-openai_compact_mode"]').element.value).toBe('force_on')
    await wrapper.get('#hosting-add-form').trigger('submit')
    await button('生成授权链接').trigger('click'); await flushPromises()
    expect(mocks.startOAuth).toHaveBeenCalledWith(17, 31)
    await button('返回配置').trigger('click')
    await wrapper.get('[data-testid="hosting-method-file"]').trigger('click')
    await wrapper.get('#hosting-add-form').trigger('submit')
    expect(wrapper.get('[data-testid="hosting-import-submit"]').text()).toBe('导入并更新')
    await wrapper.get('[data-testid="hosting-import-paste"]').trigger('click')
    await wrapper.get('#hosting-import-json').setValue('[{"refresh_token":"first"},{"refresh_token":"second"}]')
    await submitImport()
    expect(wrapper.get('[role="alert"]').text()).toContain('只能导入一个账号')
    expect(mocks.import).not.toHaveBeenCalled()
    await uploadAuthFile()
    await submitImport()
    expect(mocks.import).toHaveBeenCalledWith(expect.any(String), '原账号', 17, 31, expect.objectContaining({ notes: '保留备注', hosting_pool_id: 31 }))
  })

  it('imports dropped JSON files with shared settings and retries only failed accounts', async () => {
    mocks.import.mockResolvedValueOnce(hosted()).mockRejectedValueOnce({ message: '席位已用完' }).mockResolvedValueOnce(hosted())
    await render(); await button('导入账号').trigger('click'); await wrapper.get('#hosting-import-pool').setValue(31)
    await wrapper.get('[data-testid="hosting-import-files"]').trigger('click')
    const files = ['first', 'second'].map(token => {
      const file = new File([JSON.stringify({ refresh_token: token })], `${token}.json`, { type: 'application/json' })
      Object.defineProperty(file, 'text', { value: async () => JSON.stringify({ refresh_token: token }) })
      return file
    })
    await wrapper.get('[data-testid="json-file-dropzone"]').trigger('drop', { dataTransfer: { files } })
    expect(wrapper.get('[data-testid="json-file-dropzone"]').text()).toContain('first.json, second.json')
    await submitImport()
    expect(mocks.import).toHaveBeenCalledTimes(2)
    expect(mocks.import).toHaveBeenNthCalledWith(1, '{"refresh_token":"first"}', 'Codex 1', 0, 31, expect.objectContaining({ name: 'Codex 1', concurrency: 3, priority: 50 }))
    expect(wrapper.get('[data-testid="hosting-import-result"]').text()).toContain('席位已用完')
    expect(wrapper.get('[data-testid="hosting-import-submit"]').text()).toBe('仅重试失败账号')
    await submitImport()
    expect(mocks.import).toHaveBeenCalledTimes(3)
    expect(mocks.import.mock.calls[2][0]).toBe('{"refresh_token":"second"}')
    expect(wrapper.find('[data-testid="hosting-import-submit"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="hosting-import-result"]').text()).not.toContain('席位已用完')
    await button('继续导入').trigger('click')
    expect(wrapper.find('[data-testid="hosting-import-result"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="hosting-import-submit"]').attributes('disabled')).toBeDefined()
  })

  it('validates pasted credentials locally before starting any batch and blocks closing while saving', async () => {
    let finish: (account: HostedAccount) => void = () => {}
    mocks.import.mockImplementation(() => new Promise<HostedAccount>(resolve => { finish = resolve }))
    await render(); await button('导入账号').trigger('click'); await wrapper.get('#hosting-import-pool').setValue(31)
    await wrapper.get('[data-testid="hosting-import-paste"]').trigger('click')
    await wrapper.get('#hosting-import-json').setValue('[{"refresh_token":"first"},{"access_token":"unsupported"}]')
    await submitImport()
    expect(wrapper.get('[role="alert"]').text()).toContain('缺少 refresh_token')
    expect(mocks.import).not.toHaveBeenCalled()
    await wrapper.get('#hosting-import-json').setValue(JSON.stringify({ tokens: { refresh_token: 'valid' } }, null, 2))
    await submitImport()
    expect(button('取消').attributes('disabled')).toBeDefined()
    expect(wrapper.get('#hosting-import-pool').element.closest('fieldset')?.disabled).toBe(true)
    expect(wrapper.get('[data-testid="hosting-import-submit"]').attributes('disabled')).toBeDefined()
    finish(hosted()); await flushPromises()
    expect(wrapper.get('[data-testid="hosting-import-result"]').text()).toContain('导入成功')
    expect(wrapper.get<HTMLTextAreaElement>('#hosting-import-json').element.value).toBe('')
    expect(button('取消').attributes('disabled')).toBeUndefined()
  })

  it('preserves configuration and authorization input after a creation error', async () => {
    mocks.completeOAuth.mockRejectedValue({ message: '席位已用完' })
    await render(); await button('添加账号').trigger('click'); await configure()
    await button('生成授权链接').trigger('click'); await flushPromises()
    await wrapper.get('#hosting-callback').setValue('http://localhost:1455/auth/callback?code=code&state=state')
    await wrapper.get('#hosting-add-form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('席位已用完')
    expect(wrapper.get<HTMLTextAreaElement>('#hosting-callback').element.value).toContain('code=code')
    await button('返回配置').trigger('click')
    expect(wrapper.get<HTMLInputElement>('[data-testid="hosted-name"]').element.value).toBe('工作 Codex')
  })
})


describe('Hosting account table', () => {
  it('changes scheduling independently and retains the previous switch state on failure', async () => {
    const current = { ...overview(), accounts: [{ ...hosted(), state: 'login_required' as const, schedulable: true }] }
    mocks.overview.mockResolvedValue(current)
    mocks.change.mockRejectedValueOnce(new Error('Unable to save'))
    await render()
    const toggle = () => wrapper.get('[role="switch"]')
    expect(toggle().attributes('aria-checked')).toBe('true')
    await toggle().trigger('click'); await flushPromises()
    expect(mocks.change).toHaveBeenCalledWith(17, 'schedule-disable')
    expect(toggle().attributes('aria-checked')).toBe('true')
    expect(mocks.showError).toHaveBeenCalledWith('Unable to save')
    mocks.change.mockResolvedValue(undefined)
    mocks.overview.mockResolvedValue({ ...current, accounts: [{ ...current.accounts[0], schedulable: false }] })
    await toggle().trigger('click'); await flushPromises()
    expect(toggle().attributes('aria-checked')).toBe('false')
    expect(wrapper.text()).toContain('需要重新登录')
    await toggle().trigger('click'); await flushPromises()
    expect(mocks.change).toHaveBeenLastCalledWith(17, 'schedule-enable')
  })
  it('paginates rows and resets the bottom pagination when searching or filtering', async () => {
    mocks.overview.mockResolvedValue({ ...overview(), accounts: Array.from({ length: 25 }, (_, index) => ({ ...hosted(), id: index + 1, name: `Account ${index + 1}`, state: index === 24 ? 'paused' : 'active' })) })
    await render()
    expect(wrapper.findAll('[data-testid="hosting-account-row"]')).toHaveLength(20)
    wrapper.findComponent(Pagination).vm.$emit('update:page', 2)
    await flushPromises()
    expect(wrapper.findAll('[data-testid="hosting-account-row"]')).toHaveLength(5)
    await wrapper.get('#hosting-account-search').setValue('Account 25')
    expect(wrapper.findAll('[data-testid="hosting-account-row"]')).toHaveLength(1)
    expect(wrapper.findComponent(Pagination).props('page')).toBe(1)
    await wrapper.get('#hosting-account-state').setValue('active')
    expect(wrapper.findAll('[data-testid="hosting-account-row"]')).toHaveLength(0)
    await wrapper.get('#hosting-account-search').setValue('')
    await wrapper.get('#hosting-account-state').setValue('')
    await wrapper.get('.page-size-select select').setValue(50)
    await flushPromises()
    expect(wrapper.findAll('[data-testid="hosting-account-row"]')).toHaveLength(25)
    expect(wrapper.get('[data-testid="hosting-account-toolbar"]').find('.page-size-select').exists()).toBe(false)
    const table = wrapper.get('[data-testid="hosting-accounts-table"]').element
    expect(table.compareDocumentPosition(wrapper.findComponent(Pagination).element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('uses the shared menu for account usage and closes it after an action', async () => {
    mocks.accountUsage.mockResolvedValue({ five_hour: { utilization: 42 }, seven_day: null })
    await render()
    expect(button('编辑')).toBeDefined()
    await button('更多').trigger('click')
    expect(button('重新授权')).toBeDefined()
    expect(button('暂停')).toBeDefined()
    expect(wrapper.text()).not.toContain('创建 Spark')
    await button('查询用量').trigger('click')
    await flushPromises()
    expect(mocks.accountUsage).toHaveBeenCalledWith(17, true)
    expect(button('查询用量')).toBeUndefined()
    expect(wrapper.text()).toContain('42%')
  })
})
