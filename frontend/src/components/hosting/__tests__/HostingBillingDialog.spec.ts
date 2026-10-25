import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import HostingBillingDialog from '../HostingBillingDialog.vue'
import zh from '@/i18n/locales/zh'
import type { HostedAccount } from '@/api/hosting'
const mocks = vi.hoisted(() => ({ billing: vi.fn() }))
vi.mock('@/api/hosting', async original => ({ ...await original<typeof import('@/api/hosting')>(), hostingAPI: mocks }))
vi.mock('vue-i18n', async original => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key.split('.').reduce<unknown>((value, part) => (value as Record<string, unknown>)[part], zh) }) }))
const result = { requests: 100, tokens: 24000000, standard_cost: 100, actual_cost: 3, hosting_cost: 3, fallback_cost: 0, items: [{ id: 1, request_id: 'request-precision', input_tokens: 12345, output_tokens: 6789, input_cost: .00000123, output_cost: .00000555, cache_read_cost: 0, cache_creation_cost: 0, created_at: '2026-10-11T10:00:00Z', account_name: 'Codex', key_name: '演示', model: 'gpt-5', route: 'hosted', group_name: '', reason: '', tokens: 240000, standard_cost: 1, rate: .03, actual_cost: .03 }] }
const stubs = { BaseDialog: { template: '<div><slot/></div>' }, Pagination: true }
describe('Hosting billing', () => {
  it('shows historical actual charges and scopes the request to the selected account', async () => {
    mocks.billing.mockResolvedValue(result)
    const wrapper = mount(HostingBillingDialog, { props: { show: true, account: { id: 17, name: 'Codex' } as HostedAccount }, global: { stubs } })
    await flushPromises()
    expect(mocks.billing).toHaveBeenLastCalledWith(17, '', 1, 20, false)
    expect(wrapper.text()).toContain('3.00000000 USD')
    expect(wrapper.text()).toContain('100.00000000 USD')
    expect(wrapper.text()).toContain('0.03×')
    expect(wrapper.text()).toContain('0.03000000 USD')
    await wrapper.findAll('button').find(button => button.text() === zh.hosting.billingDetails)!.trigger('click')
    expect(wrapper.text()).toContain('request-precision')
    expect(wrapper.text()).toContain('0.00000123 USD')
    expect(wrapper.text()).toContain('0.00000555 USD')
    wrapper.unmount()
  })
  it('filters fallback billing and shows the actual group and switch reason', async () => {
    mocks.billing.mockResolvedValue(result)
    const wrapper = mount(HostingBillingDialog, { props: { show: true }, global: { stubs } })
    await flushPromises()
    mocks.billing.mockResolvedValue({ ...result, actual_cost: 1.25, hosting_cost: 0, fallback_cost: 1.25, items: [{ ...result.items[0], route: 'fallback', group_name: 'VIP', reason: '额度已用完', actual_cost: 1.25, rate: 1 }] })
    await wrapper.findAll('button').find(button => button.text() === zh.hosting.fallbackUsage)!.trigger('click')
    await flushPromises()
    expect(mocks.billing).toHaveBeenLastCalledWith(0, 'fallback', 1, 20, false)
    expect(wrapper.text()).toContain('VIP')
    expect(wrapper.text()).toContain('额度已用完')
    expect(wrapper.text()).toContain('1.25000000 USD')
    wrapper.unmount()
  })
})
