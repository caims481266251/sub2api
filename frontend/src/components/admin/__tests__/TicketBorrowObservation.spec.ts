import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TicketBorrowObservation from '../TicketBorrowObservation.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))

describe('borrowing candidate observation', () => {
  it('distinguishes pre-verification age from recorded age and retains zero values', () => {
    const w = mount(TicketBorrowObservation, { props: { details: {
      candidate_id: 'candidate-a', batch_id: 'batch-a', source: {}, target: {},
      cookie_age_at_probe_ms: 0, remaining_at_probe_ms: 229500, cookie_age_ms: 3400,
      probe_verdict: 'degraded', mint_status: 200, continue_status: 200
    } } })
    expect(w.get('[data-observation-field="cookieAgeAtProbe"]').text()).toContain('0.000 s')
    expect(w.get('[data-observation-field="cookieAgeAtRecord"]').text()).toContain('3.400 s')
    expect(w.get('[data-observation-field="remainingAtProbe"]').text()).toContain('229.500 s')
    expect(w.get('[data-observation-field="probeRetries"]').text()).toContain('0')
    expect(w.findAll('[data-observation-field="httpStatus"] [data-tag-value="200"]')).toHaveLength(2)
    expect(w.get('[data-observation-field="probeVerdict"]').text()).toContain('ticketBorrowing.tags.ticketChanged')
    expect(w.get('[data-observation-field="probeVerdict"] [data-tag-tone]').attributes('data-tag-tone')).toBe('danger')
  })

  it('does not invent verification values for an unused source candidate', () => {
    const w = mount(TicketBorrowObservation, { props: { details: {
      candidate_id: 'candidate-b', batch_id: 'batch-b', source: { client_version: '0.162.1' }, target: {}, cookie_age_ms: 800
    } } })
    expect(w.find('[data-observation-field="cookieAgeAtProbe"]').exists()).toBe(false)
    expect(w.find('[data-observation-field="probeRetries"]').exists()).toBe(false)
    expect(w.get('[data-observation-field="sourceVersion"]').text()).toContain('0.162.1')
  })
})
