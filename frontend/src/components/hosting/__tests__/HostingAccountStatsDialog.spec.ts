import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import HostingAccountStatsDialog from '../HostingAccountStatsDialog.vue'
import AccountStatsModal from '@/components/admin/account/AccountStatsModal.vue'
import ModelDistributionChart from '@/components/charts/ModelDistributionChart.vue'
import EndpointDistributionChart from '@/components/charts/EndpointDistributionChart.vue'
import type { HostedAccount } from '@/api/hosting'
import type { AccountUsageStatsResponse } from '@/types'

const mocks = vi.hoisted(() => ({ accountStats: vi.fn(), getStats: vi.fn() }))
vi.mock('@/api/hosting', () => ({ hostingAPI: mocks }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getStats: mocks.getStats } } }))
vi.mock('vue-chartjs', () => ({ Line: { template: '<div />' }, Bar: { template: '<div />' }, Doughnut: { template: '<div />' } }))
vi.mock('vue-i18n', async original => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

const account = { id: 25, name: 'Hosted Codex', state: 'active' } as HostedAccount
const result = {
  history: [{ date: '2026-10-11', label: '10/11', requests: 1, tokens: 200, cost: 100, actual_cost: 100, user_cost: 3.00012345 }],
  summary: { days: 30, actual_days_used: 1, total_cost: 100, total_standard_cost: 100, total_user_cost: 3.00012345, total_requests: 1, total_tokens: 200, avg_daily_cost: 100, avg_daily_user_cost: 3.00012345, avg_daily_requests: 1, avg_daily_tokens: 200, avg_duration_ms: 100 },
  models: [], endpoints: [], upstream_endpoints: []
} as unknown as AccountUsageStatsResponse
const stubs = {
  BaseDialog: { props: ['show'], template: '<div v-if="show"><slot/><slot name="footer"/></div>' },
  Line: true, ModelDistributionChart: true, EndpointDistributionChart: true,
  HostingBillingDialog: { props: ['show', 'account', 'admin'], emits: ['close'], template: '<div v-if="show" data-test="billing"><button @click="$emit(\'close\')">Back</button></div>' }
}

describe('Hosted account statistics', () => {
  beforeEach(() => { vi.clearAllMocks(); mocks.accountStats.mockResolvedValue(result) })

  it('reuses admin statistics with an owner-scoped loader, exact costs and safe charts', async () => {
    const wrapper = mount(HostingAccountStatsDialog, { props: { show: true, account }, global: { stubs } })
    await flushPromises()
    expect(mocks.accountStats).toHaveBeenCalledWith(25, 30, false)
    expect(mocks.getStats).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('$100.00000000')
    expect(wrapper.text()).toContain('$3.00012345')
    expect(wrapper.text()).toContain('admin.accounts.stats.todayOverview')
    expect(wrapper.text()).toContain('admin.accounts.stats.performance')
    expect(wrapper.findComponent(ModelDistributionChart).props()).toMatchObject({ enableBreakdown: false, showAccountCost: false, costPrecision: 8 })
    for (const chart of wrapper.findAllComponents(EndpointDistributionChart)) expect(chart.props()).toMatchObject({ enableBreakdown: false, costPrecision: 8 })
    await wrapper.findAll('button').find(b => b.text() === 'hosting.billingDetails')!.trigger('click')
    expect(wrapper.find('[data-test="billing"]').exists()).toBe(true)
    await wrapper.get('[data-test="billing"] button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.accounts.stats.usageTrend')
    wrapper.unmount()
  })

  it('uses the admin hosted endpoint when opened from hosted account management', async () => {
    const wrapper = mount(HostingAccountStatsDialog, { props: { show: true, account, admin: true }, global: { stubs } })
    await flushPromises()
    expect(mocks.accountStats).toHaveBeenCalledWith(25, 30, true)
    wrapper.unmount()
  })

  it('discards stale responses when switching accounts and supports retry', async () => {
    let resolveFirst!: (value: AccountUsageStatsResponse) => void
    const loader = vi.fn().mockImplementationOnce(() => new Promise(resolve => { resolveFirst = resolve })).mockRejectedValueOnce(new Error('offline')).mockResolvedValue(result)
    const wrapper = mount(AccountStatsModal, { props: { show: true, account: { id: 25, name: 'A', status: 'active' }, hosting: true, loadAccountStats: loader }, global: { stubs } })
    await wrapper.setProps({ account: { id: 26, name: 'B', status: 'active' } })
    await flushPromises()
    resolveFirst(result)
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('$100.00000000')
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(loader).toHaveBeenLastCalledWith(26, 30)
    expect(wrapper.text()).toContain('$100.00000000')
    wrapper.unmount()
  })

  it('keeps the original admin loader and formatting for platform accounts', async () => {
    mocks.getStats.mockResolvedValue(result)
    const wrapper = mount(AccountStatsModal, { props: { show: true, account: { id: 1, name: 'Platform', status: 'active' } }, global: { stubs } })
    await flushPromises()
    expect(mocks.getStats).toHaveBeenCalledWith(1, 30)
    expect(wrapper.text()).toContain('$100.00')
    expect(wrapper.text()).not.toContain('$100.00000000')
    wrapper.unmount()
  })
})
