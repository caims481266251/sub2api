import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AccountUsageInfo } from '@/types'
import type { HostedAccount } from '@/api/hosting'
import { hostingAPI } from '@/api/hosting'
import { refreshOpenAIQuota, resetOpenAIQuota } from '@/api/admin/accounts'
import HostingAccountUsage from '../HostingAccountUsage.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import UsageProgressBar from '@/components/account/UsageProgressBar.vue'

vi.mock('@/api/hosting', async original => ({
  ...await original<typeof import('@/api/hosting')>(),
  hostingAPI: { accountUsage: vi.fn(), refreshQuota: vi.fn(), resetQuota: vi.fn() },
}))
vi.mock('@/api/admin/accounts', () => ({ refreshOpenAIQuota: vi.fn(), resetOpenAIQuota: vi.fn() }))
vi.mock('vue-i18n', async original => ({
  ...await original<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

const usage = (percent: number): AccountUsageInfo => ({
  updated_at: null, seven_day_sonnet: null,
  five_hour: { utilization: percent, resets_at: '2099-01-01T00:00:00Z', remaining_seconds: 3600 },
  seven_day: { utilization: 92.4, resets_at: '2099-01-03T00:00:00Z', remaining_seconds: 172800 },
})
const account = (id = 17): HostedAccount => ({
  id, name: 'Codex', email: 'demo@example.com', state: 'active', reset_at: null, last_used_at: null,
  usage_5h_percent: 25.6, usage_7d_percent: 92.4, usage: usage(25.6),
  settings: { name: 'Codex', hosting_pool_id: 0, concurrency: 3, priority: 50, credentials: {}, extra: {} },
})
const quota = {
  fetched_at: 1, cache_persisted: true, credits_cache_persisted: true,
  credits: { has_credits: true, unlimited: false, balance: '12.3456' },
  rate_limit_reset_credits: { available_count: 2, credits: [{ expires_at: '2099-01-01T00:00:00Z' }, { expires_at: '2099-01-02T00:00:00Z' }] },
}
let wrapper: ReturnType<typeof mount>
const button = (key: string) => wrapper.findAll('button').find(item => item.text().includes(key))!
function render(admin = false) {
  wrapper = mount(HostingAccountUsage, { props: { account: account(), admin }, global: { stubs: { ConfirmDialog: true } } })
}
beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(hostingAPI.accountUsage).mockResolvedValue(usage(40))
  vi.mocked(hostingAPI.refreshQuota).mockResolvedValue(quota)
  vi.mocked(hostingAPI.resetQuota).mockResolvedValue({ code: 'ok', windows_reset: 2, quota: { ...quota, rate_limit_reset_credits: { available_count: 1 } }, cache_refreshed: true, account_state_recovered: true, account: { ...account(), usage: usage(0) } })
})
afterEach(() => wrapper?.unmount())

describe('Hosted account quota panel', () => {
  it('uses the same rounded percentages, colors and individual reset times as admin accounts', () => {
    render()
    expect(wrapper.text()).toContain('26%')
    expect(wrapper.text()).toContain('92%')
    expect(wrapper.find('.bg-red-500').exists()).toBe(true)
    const bars = wrapper.findAllComponents(UsageProgressBar)
    expect(bars.map(bar => bar.props('label'))).toEqual(['5h', '7d'])
    expect(bars.map(bar => bar.props('resetsAt'))).toEqual(['2099-01-01T00:00:00Z', '2099-01-03T00:00:00Z'])
    expect(hostingAPI.accountUsage).not.toHaveBeenCalled()
    expect(wrapper.text()).not.toContain('openaiReferral')
  })

  it.each([false, true])('queries through the scoped hosting API (admin=%s)', async admin => {
    render(admin)
    await button('usageWindow.activeQuery').trigger('click'); await flushPromises()
    expect(hostingAPI.accountUsage).toHaveBeenCalledWith(17, true, admin)
    expect(wrapper.text()).toContain('40%')
    await button('openaiQuotaReset.count').trigger('click'); await flushPromises()
    expect(hostingAPI.refreshQuota).toHaveBeenCalledWith(17, admin)
    expect(wrapper.text()).toContain('12.3456')
    expect(button('openaiQuotaReset.count').text()).toContain('2')
    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
  })

  it('requires confirmation to consume a reset credit and updates the existing row', async () => {
    render()
    expect(button('openaiQuotaReset.reset').attributes('disabled')).toBeDefined()
    await button('openaiQuotaReset.count').trigger('click'); await flushPromises()
    await button('openaiQuotaReset.reset').trigger('click')
    expect(wrapper.getComponent(ConfirmDialog).props('show')).toBe(true)
    expect(hostingAPI.resetQuota).not.toHaveBeenCalled()
    wrapper.getComponent(ConfirmDialog).vm.$emit('confirm'); await flushPromises()
    expect(hostingAPI.resetQuota).toHaveBeenCalledTimes(1)
    expect(hostingAPI.resetQuota).toHaveBeenCalledWith(17, false)
    expect(wrapper.emitted('updated')?.[0]?.[0]).toMatchObject({ id: 17, usage: { five_hour: { utilization: 0 } } })
    expect(button('openaiQuotaReset.count').text()).toContain('1')
    expect(resetOpenAIQuota).not.toHaveBeenCalled()
  })

  it('keeps existing usage visible on query failure and allows retry', async () => {
    vi.mocked(hostingAPI.accountUsage).mockRejectedValueOnce(new Error('Query unavailable'))
    render()
    await button('usageWindow.activeQuery').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('Query unavailable')
    expect(wrapper.text()).toContain('26%')
    await button('usageWindow.activeQuery').trigger('click'); await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('40%')
  })

  it('discards a delayed usage response when the row switches to another account', async () => {
    let finish!: (value: AccountUsageInfo) => void
    vi.mocked(hostingAPI.accountUsage).mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    render()
    await button('usageWindow.activeQuery').trigger('click')
    await wrapper.setProps({ account: { ...account(18), usage: usage(15) } })
    finish(usage(99)); await flushPromises()
    expect(wrapper.text()).toContain('15%')
    expect(wrapper.text()).not.toContain('99%')
  })

  it('does not invent a zero percentage when no quota snapshot is available', () => {
    render()
    return wrapper.setProps({ account: { ...account(), usage: { updated_at: null, five_hour: null, seven_day: null, seven_day_sonnet: null }, usage_5h_percent: null, usage_7d_percent: null } }).then(() => {
      expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
      expect(button('usageWindow.activeQuery').exists()).toBe(true)
    })
  })
})
