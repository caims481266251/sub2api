import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import HostingSettingsView from '../HostingSettingsView.vue'
import HostingAccountsView from '../HostingAccountsView.vue'
import HostingUsersView from '../HostingUsersView.vue'
import zh from '@/i18n/locales/zh'

const api = vi.hoisted(() => ({ settings: vi.fn(), saveSettings: vi.fn(), adminAccounts: vi.fn(), users: vi.fn(), policy: vi.fn(), savePolicy: vi.fn(), adminAccountPools: vi.fn(), change: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/hosting', async original => ({ ...await original<typeof import('@/api/hosting')>(), hostingAPI: api }))
vi.mock('@/api/admin/users', () => ({ usersAPI: { list: api.users }, default: { list: api.users } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: api.showSuccess }) }))
vi.mock('@/components/common/Select.vue', () => ({ default: {
  props: ['modelValue', 'options', 'disabled'], emits: ['update:modelValue'],
  template: '<select :value="modelValue" :disabled="disabled" @change="$emit(\'update:modelValue\', Number($event.target.value))"><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>',
} }))
vi.mock('vue-i18n', async original => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string, params: Record<string, unknown> = {}) => {
  let value: unknown = zh
  for (const part of key.split('.')) value = value && typeof value === 'object' ? (value as Record<string, unknown>)[part] : undefined
  if (typeof value !== 'string') throw new Error(`Missing translation: ${key}`)
  return value.replace(/\{(\w+)\}/g, (match, name) => String(params[name] ?? match))
} }) }))
let wrapper: ReturnType<typeof mount>
const account = (id: number, owner: string, state: string) => ({ id, user_id: id, username: owner, user_email: `${owner}@example.com`, name: `Codex ${owner}`, email: `${owner}@openai.example`, pool_name: id === 1 ? '工作池' : '', state, last_used_at: null, reset_at: null, usage_5h_percent: 20, usage_7d_percent: 30, current_concurrency: 2, current_rpm: 4, schedulable: true, settings: { hosting_pool_id: null, name: `Codex ${owner}`, concurrency: 8, priority: 40, credentials: {}, extra: { base_rpm: 30 } } })
const rows = [account(1, 'alice', 'active'), { ...account(2, 'bob', 'login_required'), error_message: '401 Unauthorized' }]
const user = { id: 1, username: 'alice', email: 'alice@example.com', status: 'active' }
const button = (text: string) => wrapper.findAll('button').find(item => item.text() === text)!
async function render(component: typeof HostingAccountsView | typeof HostingSettingsView | typeof HostingUsersView, path: string) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div/>' } }] })
  await router.push(path); await router.isReady()
  wrapper = mount(component, { global: { plugins: [router], stubs: {
    Teleport: true, AppLayout: { template: '<div><slot/></div>' },
    BaseDialog: { props: ['show'], template: '<div v-if="show" data-testid="policy-dialog"><slot/><slot name="footer"/></div>' },
    Pagination: { props: ['page', 'total'], emits: ['update:page'], template: '<button data-testid="next-page" @click="$emit(\'update:page\', page + 1)">Next</button>' },
  } } }); await flushPromises()
}
beforeEach(() => {
  Object.values(api).forEach(mock => mock.mockReset())
  api.adminAccountPools.mockResolvedValue([{ id: 10, name: '工作池', account_count: 1, key_count: 0 }])
  api.change.mockResolvedValue(undefined)
  api.settings.mockResolvedValue({ enabled: true, default_seats: 1, rate_multiplier: .03 })
  api.users.mockResolvedValue({ items: [user], total: 21 })
  api.adminAccounts.mockImplementation(async (page: number, size: number) => ({ items: rows, total: 21, page, page_size: size }))
  api.policy.mockResolvedValue({ policy: { seat_limit: null, rate_multiplier: null }, seat_limit: 1, rate_multiplier: .03 })
  api.saveSettings.mockResolvedValue(undefined); api.savePolicy.mockResolvedValue(undefined)
})
afterEach(() => wrapper?.unmount())

describe('Separate hosting administration pages', () => {
  it('shows account ownership, pool, quota and availability without loading global configuration or users', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts')
    expect(api.adminAccounts).toHaveBeenCalledWith(1, 20, '')
    for (const text of ['alice@example.com', 'bob@example.com', '工作池', '默认托管池', '错误', '5h', '7d', '20%', '30%']) expect(wrapper.text()).toContain(text)
    expect(wrapper.find('#hosting-default-rate').exists()).toBe(false)
    expect(wrapper.find('#hosting-user-search').exists()).toBe(false)
    expect(api.settings).not.toHaveBeenCalled(); expect(api.users).not.toHaveBeenCalled()
    expect(wrapper.get('a[aria-current="page"]').attributes('href')).toBe('/admin/hosting/accounts')
  })
  it('shows actual concurrency and RPM and opens the account owner configuration', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts')
    const capacity = wrapper.findAll('[data-testid="hosting-capacity"]')[0]
    expect(capacity.text()).toContain('2/8')
    expect(capacity.text()).toContain('4/30')
    await button('编辑').trigger('click'); await flushPromises()
    expect(api.adminAccountPools).toHaveBeenCalledWith(1)
    expect(wrapper.find('#hosted-concurrency').element).toHaveProperty('value', '8')
  })
  it('offers intelligence testing for administrators', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts')
    await button('更多').trigger('click'); await flushPromises()
    expect(button(zh.admin.accounts.pelicanTest.menu)).toBeDefined()
  })

  it('pauses through the hosting admin endpoint and reloads the current page', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts')
    await button('更多').trigger('click'); await button('暂停').trigger('click'); await flushPromises()
    expect(api.change).toHaveBeenCalledWith(1, 'pause', true)
    expect(api.adminAccounts).toHaveBeenCalledTimes(2)
  })

  it('changes scheduling through the owner-scoped admin endpoint', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts')
    await wrapper.findAll('[role="switch"]')[0].trigger('click'); await flushPromises()
    expect(api.change).toHaveBeenCalledWith(1, 'schedule-disable', true)
  })
  it('paginates, changes the page size and keeps searches explicit', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts')
    await wrapper.get('[data-testid="next-page"]').trigger('click'); await flushPromises()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(2, 20, '')
    await wrapper.get('#hosting-account-search').setValue(' bob@example.com ')
    await button('刷新').trigger('click'); await flushPromises()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(2, 20, '')
    await wrapper.get('#hosting-account-search-form').trigger('submit'); await flushPromises()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(1, 20, 'bob@example.com')
    await wrapper.get('[data-testid="hosting-page-size"]').setValue(100); await flushPromises()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(1, 100, 'bob@example.com')
    await button('清除搜索').trigger('click'); await flushPromises()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(1, 100, '')
  })
  it('accepts a linked user filter and filters directly from an owner row', async () => {
    await render(HostingAccountsView, '/admin/hosting/accounts?user_id=1')
    expect(api.adminAccounts).toHaveBeenCalledWith(1, 20, '1')
    expect(wrapper.get<HTMLInputElement>('#hosting-account-search').element.value).toBe('alice@example.com')
    await button('bob').trigger('click'); await flushPromises()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(1, 20, '2')
    expect(wrapper.get<HTMLInputElement>('#hosting-account-search').element.value).toBe('bob@example.com')
    expect(wrapper.vm.$route.query.user_id).toBe('2')
    await button('清除搜索').trigger('click'); await flushPromises()
    expect(wrapper.vm.$route.query.user_id).toBeUndefined()
    expect(api.adminAccounts).toHaveBeenLastCalledWith(1, 20, '')
  })
  it('shows list errors and lets the administrator retry without loading configuration', async () => {
    api.adminAccounts.mockRejectedValueOnce({ message: 'List unavailable' })
    await render(HostingAccountsView, '/admin/hosting/accounts')
    expect(wrapper.get('[role="alert"]').text()).toBe('List unavailable')
    await button('刷新').trigger('click'); await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('alice@example.com')
    expect(api.settings).not.toHaveBeenCalled()
  })
  it('loads and saves only global settings on the configuration page', async () => {
    await render(HostingSettingsView, '/admin/hosting/settings')
    expect(api.settings).toHaveBeenCalledTimes(1)
    expect(api.adminAccounts).not.toHaveBeenCalled(); expect(api.users).not.toHaveBeenCalled()
    expect(wrapper.find('#hosting-account-search').exists()).toBe(false)
    await wrapper.get('#hosting-default-seats').setValue(3)
    await wrapper.get('#hosting-default-rate').setValue(.05)
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(api.saveSettings).toHaveBeenCalledWith({ enabled: true, default_seats: 3, rate_multiplier: .05 })
  })
  it('paginates users and opens policy settings on demand with no account or global list request', async () => {
    await render(HostingUsersView, '/admin/hosting/users')
    expect(api.users).toHaveBeenCalledWith(1, 20, { search: '' })
    expect(api.policy).not.toHaveBeenCalled(); expect(api.adminAccounts).not.toHaveBeenCalled(); expect(api.settings).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="next-page"]').trigger('click'); await flushPromises()
    expect(api.users).toHaveBeenLastCalledWith(2, 20, { search: '' })
    await wrapper.get('#hosting-user-search').setValue(' alice ')
    await wrapper.get('#hosting-user-search-form').trigger('submit'); await flushPromises()
    expect(api.users).toHaveBeenLastCalledWith(1, 20, { search: 'alice' })
    expect(wrapper.get('a[href="/admin/hosting/accounts?user_id=1"]').exists()).toBe(true)
    await button('设置席位与倍率').trigger('click'); await flushPromises()
    expect(api.policy).toHaveBeenCalledWith(1)
    expect(wrapper.get('[data-testid="policy-dialog"]').text()).toContain('alice@example.com')
    await wrapper.get('#hosting-user-seats').setValue(0)
    await wrapper.get('#hosting-user-rate').setValue('')
    await wrapper.get('#hosting-user-policy-form').trigger('submit'); await flushPromises()
    expect(api.savePolicy).toHaveBeenCalledWith(1, { seat_limit: 0, rate_multiplier: null })
    expect(wrapper.find('[data-testid="policy-dialog"]').exists()).toBe(false)
  })
  it('allows policy editing from an account row and retains changes after save errors', async () => {
    api.savePolicy.mockRejectedValueOnce({ message: 'Save unavailable' })
    await render(HostingAccountsView, '/admin/hosting/accounts')
    await button('更多').trigger('click'); await button('用户设置').trigger('click'); await flushPromises()
    expect(api.policy).toHaveBeenCalledWith(1)
    await wrapper.get('#hosting-user-rate').setValue(0)
    await wrapper.get('#hosting-user-policy-form').trigger('submit'); await flushPromises()
    expect(api.savePolicy).toHaveBeenCalledWith(1, { seat_limit: null, rate_multiplier: 0 })
    expect(wrapper.get('[role="alert"]').text()).toBe('Save unavailable')
    expect(wrapper.get<HTMLInputElement>('#hosting-user-rate').element.value).toBe('0')
  })
})
