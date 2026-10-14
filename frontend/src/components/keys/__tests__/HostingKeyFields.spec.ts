import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Group } from '@/types'
import HostingKeyFields from '../HostingKeyFields.vue'
import { hostingErrorMessage, hostingFallbackGroups } from '@/api/hosting'

const { status } = vi.hoisted(() => ({ status: vi.fn() }))
vi.mock('@/api/hosting', async importOriginal => ({ ...await importOriginal<typeof import('@/api/hosting')>(), hostingAPI: { status } }))
vi.mock('vue-i18n', async importOriginal => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

const groups = [
  { id: 9, name: 'OpenAI balance', platform: 'openai', subscription_type: 'standard', status: 'active', rate_multiplier: 2 },
  { id: 10, name: 'Subscription', platform: 'openai', subscription_type: 'subscription', status: 'active' },
  { id: 11, name: 'Disabled', platform: 'openai', subscription_type: 'standard', status: 'inactive' },
  { id: 12, name: 'Other', platform: 'anthropic', subscription_type: 'standard', status: 'active' },
] as Group[]
const render = (routeMode: 'group' | 'hosted' = 'group') => mount(HostingKeyFields, {
  props: { routeMode, fallbackEnabled: true, fallbackGroupId: routeMode === 'hosted' ? 9 : null, groups },
  global: { stubs: { GroupBadge: true } },
})

describe('Hosted key configuration', () => {
  it('selects a private hosted pool independently from the fallback group', async () => {
    status.mockResolvedValue({ enabled: true, rate_multiplier: .03, pools: [{ id: 0, name: '', account_count: 1, key_count: 0 }, { id: 31, name: '工作池', account_count: 2, key_count: 1 }] })
    const wrapper = render('hosted')
    await flushPromises()
    await wrapper.get('#hosting-pool').setValue('31')
    expect(wrapper.emitted('update:poolId')).toEqual([[31]])
    expect(wrapper.get<HTMLSelectElement>('#hosting-fallback-group').element.value).toBe('9')
    expect(wrapper.emitted('update:fallbackGroupId')).toBeUndefined()
    wrapper.unmount()
  })
  beforeEach(() => { status.mockReset(); status.mockResolvedValue({ enabled: true, rate_multiplier: .03, default_fallback_group_id: 9 }) })
  it('requires the user to choose a fallback when choosing the hosted pool', async () => {
    const wrapper = render()
    await flushPromises()
    await wrapper.get('input[value="hosted"]').setValue()
    expect(wrapper.emitted('update:routeMode')).toEqual([['hosted']])
    expect(wrapper.emitted('update:fallbackEnabled')).toEqual([[true]])
    expect(wrapper.emitted('update:fallbackGroupId')).toEqual([[null]])
    wrapper.unmount()
  })
  it('keeps existing key configuration when the administrator changes the hosting rate', async () => {
    status.mockResolvedValue({ enabled: true, rate_multiplier: .08, default_fallback_group_id: 12 })
    const wrapper = render('hosted')
    await flushPromises()
    expect(wrapper.get<HTMLSelectElement>('#hosting-fallback-group').element.value).toBe('9')
    expect(wrapper.emitted('update:fallbackGroupId')).toBeUndefined()
    expect(wrapper.findAll('#hosting-fallback-group option').map(o => o.element.value)).toEqual(['', '9'])
    await wrapper.get('[data-testid="hosting-fallback-toggle"]').trigger('click')
    expect(wrapper.emitted('update:fallbackEnabled')).toEqual([[false]])
    wrapper.unmount()
  })
  it('lets the user choose only eligible groups', async () => {
    status.mockResolvedValue({ enabled: true, rate_multiplier: .03, default_fallback_group_id: 10 })
    const wrapper = render()
    await flushPromises()
    await wrapper.get('input[value="hosted"]').setValue()
    expect(wrapper.emitted('update:fallbackGroupId')).toEqual([[null]])
    await wrapper.setProps({ routeMode: 'hosted', fallbackGroupId: null })
    await wrapper.get('#hosting-fallback-group').setValue('9')
    expect(wrapper.emitted('update:fallbackGroupId')).toEqual([[null], [9]])
    wrapper.unmount()
  })
  it('prevents selecting hosted mode while the feature is disabled', async () => {
    status.mockResolvedValue({ enabled: false, rate_multiplier: .03 })
    const wrapper = render('hosted')
    await flushPromises()
    expect(wrapper.get('input[value="hosted"]').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
  it('filters fallback eligibility and preserves server error details', () => {
    expect(hostingFallbackGroups(groups).map(g => g.id)).toEqual([9])
    expect(hostingErrorMessage({ message: '席位已用完' }, 'failed')).toBe('席位已用完')
    expect(hostingErrorMessage(null, 'failed')).toBe('failed')
  })
})
