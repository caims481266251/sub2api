import { mount } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import BorrowAttemptDetails from './BorrowAttemptDetails.vue'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
describe('BorrowAttemptDetails', () => {
  it('shows actual route, retry budget and independent reasoning/text observations', () => {
    const wrapper = mount(BorrowAttemptDetails, { props: { rows: [{
      attempt: { kind: 'egress', number: 3, account_id: 116, proxy_id: 5, borrow_route_id: 'route-b', model: 'gpt-6.1-sol', reasoning_effort: 'high', status: 200, start_ms: 1000, end_ms: 63000, reused: true, request_bytes: 2, response_bytes: 40, body_eof: false, no_text_long_tail: true, reasoning_events: 36, events: { request_written: 1100, first_reasoning: 2000 } },
      recovery: { transport_attempts: 2, requests_sent: 2, recovering: true, output_started: true, stop_reason: 'output_started' }
    }] } })
    expect(wrapper.text()).toContain('route-b')
    expect(wrapper.text()).toContain('gpt-6.1-sol / high')
    expect(wrapper.text()).toContain('requestTiming.noTextLongTail')
    expect(wrapper.text()).toContain('2 / 2')
    expect(wrapper.text()).toContain('1.00 s · 36')
    expect(wrapper.text()).toContain('requestTiming.firstText: —')
  })
  it('does not show an empty observation panel for historical records', () => {
    expect(mount(BorrowAttemptDetails, { props: { rows: [] } }).find('section').exists()).toBe(false)
  })
})
