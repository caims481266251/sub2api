import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import HostingAccountStatus from '../HostingAccountStatus.vue'
import HostingAccountActions from '../HostingAccountActions.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'
import type { HostedAccount } from '@/api/hosting'

const mocks = vi.hoisted(() => ({ tempState: vi.fn(), operation: vi.fn(), showSuccess: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/hosting', async original => ({ ...await original<typeof import('@/api/hosting')>(), hostingAPI: mocks }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ ...mocks, cachedPublicSettings: {} }) }))
vi.mock('vue-i18n', async original => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

const account: HostedAccount = {
  id: 17, name: 'Hosted', email: '', status: 'active', state: 'temporarily_unavailable', schedulable: true,
  reset_at: null, last_used_at: null, usage_5h_percent: null, usage_7d_percent: null,
  temp_unschedulable_until: '2099-01-01T00:00:00Z',
  settings: { hosting_pool_id: 0, name: 'Hosted', concurrency: 3, priority: 10, credentials: {}, extra: {} }
}
const stubs = { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot/><slot name="footer"/></div>' }, HelpTooltip: true }
describe('Hosted account runtime and operations', () => {
  it('opens the shared temporary-block details and recovers via the owned API', async () => {
    mocks.tempState.mockResolvedValue({ active: true, state: { until_unix: 4070908800, triggered_at_unix: 1791700000, status_code: 429, matched_keyword: 'quota', rule_index: 0, error_message: 'Quota exceeded' } })
    mocks.operation.mockResolvedValue({ ...account, temp_unschedulable_until: null })
    const wrapper = mount(HostingAccountStatus, { props: { account }, global: { stubs } })
    expect(wrapper.text()).toContain('admin.accounts.status.tempUnschedulable')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(mocks.tempState).toHaveBeenCalledWith(17, false)
    expect(wrapper.text()).toContain('Quota exceeded')
    await wrapper.findAll('button').find(b => b.text() === 'admin.accounts.recoverState')!.trigger('click')
    await flushPromises()
    expect(mocks.operation).toHaveBeenCalledWith(17, 'recover-state', false)
    expect(wrapper.emitted('updated')).toHaveLength(1)
    wrapper.unmount()
  })
  it('shows shared RPM pause and rate-limit statuses', async () => {
    const wrapper = mount(HostingAccountStatus, { props: { account: { ...account, state: 'active', temp_unschedulable_until: null, current_rpm: 20, settings: { ...account.settings, extra: { base_rpm: 20 } } } }, global: { stubs } })
    expect(wrapper.text()).toContain('admin.accounts.status.rpmPaused')
    await wrapper.setProps({ account: { ...account, rate_limit_reset_at: '2099-01-01T00:00:00Z' } })
    expect(wrapper.text()).toContain('admin.accounts.status.rateLimited')
    wrapper.unmount()
  })
  it('routes maintenance through hosting and preserves existing admin page metadata on update', async () => {
    mocks.operation.mockResolvedValue(account)
    const wrapper = mount(HostingAccountActions, { props: { account, admin: true }, global: { stubs: { ...stubs, AccountActionMenu: true, ConfirmDialog: true } } })
    wrapper.findComponent(AccountActionMenu).vm.$emit('refresh-token')
    await flushPromises()
    expect(mocks.operation).toHaveBeenCalledWith(17, 'refresh', true)
    expect(wrapper.emitted('updated')?.[0]).toEqual([account])
    wrapper.unmount()
  })
})
