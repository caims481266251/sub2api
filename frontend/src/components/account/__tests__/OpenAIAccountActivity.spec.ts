import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OpenAIAccountActivity from '../OpenAIAccountActivity.vue'
import type { AccountActivity, AccountRecentRequest } from '@/api/admin/accountActivity'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

function request(id: number, overrides: Partial<AccountRecentRequest> = {}): AccountRecentRequest {
  return { attempt_id: String(id), account_id: 1, source: 'business', model: `model-${id}`, completed_at: '2026-10-25T01:00:00Z',
    outcome: 'success', http_status: 200, first_token_ms: 1000, duration_ms: 2000, ...overrides }
}
function activity(recent: AccountRecentRequest[] | null): AccountActivity {
  return { account_id: 1, quality: { account_id: 1, scope: 'scope', state: 'untested', enabled: false, model: 'gpt-5', interval_minutes: 5 }, recent_requests: recent }
}
function render(recent: AccountRecentRequest[] | null) {
  return mount(OpenAIAccountActivity, {
    props: { activity: activity(recent) },
    global: { stubs: { Icon: true, HelpTooltip: { template: '<div><slot name="trigger" /><slot /></div>' } } }
  })
}

describe('OpenAIAccountActivity', () => {
  it('renders exactly 16 slots, padding on the left and keeping the newest request on the right', async () => {
    const wrapper = render([request(1), request(2)])
    expect(wrapper.findAll('[data-testid="recent-request-empty"]')).toHaveLength(14)
    expect(wrapper.findAll('[data-testid="recent-request"]')).toHaveLength(2)
    expect(wrapper.get('[role="group"]').element.firstElementChild!.getAttribute('data-testid')).toBe('recent-request-empty')
    await wrapper.setProps({ activity: activity(Array.from({ length: 18 }, (_, i) => request(i + 1))) })
    const cells = wrapper.findAll('[data-testid="recent-request"]')
    expect(cells).toHaveLength(16)
    expect(cells[0]!.attributes('aria-label')).toContain('model-3')
    expect(cells[15]!.attributes('aria-label')).toContain('model-18')
    wrapper.unmount()
  })

  it('uses existing 10/30/60-second bands while distinguishing failure and cancellation from slow success', () => {
    const samples = [9999, 10000, 30000, 60000, null].map((first_token_ms, i) => request(i, { first_token_ms }))
    samples.push(request(5, { outcome: 'failure', first_token_ms: 1, http_status: 502 }), request(6, { outcome: 'cancelled', first_token_ms: 90000 }), request(7, { outcome: 'unknown' }))
    const wrapper = render(samples)
    const cells = wrapper.findAll('[data-testid="recent-request"]')
    expect(cells.map(cell => cell.attributes('data-tone'))).toEqual(['good', 'warn', 'slow', 'critical', 'good', 'critical', 'unknown', 'unknown'])
    expect(cells[3]!.attributes('data-outcome')).toBe('success')
    expect(cells[3]!.classes()).not.toContain('request-failure')
    expect(cells[5]!.classes()).toContain('request-failure')
    expect(cells[4]!.attributes('aria-label')).toContain('—')
    expect(wrapper.text()).toContain('admin.accounts.activity.unmeasured')
    wrapper.unmount()
  })

  it('distinguishes no traffic, unavailable cache, and stale data and opens quality details', async () => {
    const wrapper = render([])
    expect(wrapper.text()).toContain('admin.accounts.activity.noRequests')
    await wrapper.setProps({ activity: activity(null) })
    expect(wrapper.text()).toContain('admin.accounts.activity.unavailable')
    await wrapper.setProps({ stale: true })
    expect(wrapper.text()).toContain('admin.accounts.activity.stale')
    await wrapper.get('[data-testid="open-quality-monitor"]').trigger('click')
    expect(wrapper.emitted('open-quality')).toHaveLength(1)
    wrapper.unmount()
  })
})
