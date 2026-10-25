import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TicketBorrowObservation from '../TicketBorrowObservation.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))

describe('borrowing candidate observation', () => {
  it('distinguishes pre-verification age from recorded age and retains zero values', () => {
    const w = mount(TicketBorrowObservation, { props: { details: {
      candidate_id: 'candidate-a', batch_id: 'batch-a', source: {}, target: {},
      cookie_age_at_probe_ms: 0, remaining_at_probe_ms: 229500, cookie_age_ms: 3400,
      gateway_priority: 'prioritized', source_queue_ms: 4200, source_network_ms: 1200,
      probe_verdict: 'degraded', mint_status: 200, continue_status: 200
    } } })
    expect(w.get('[data-observation-field="gatewayPriority"]').text()).toContain('ticketBorrowing.tags.gatewayPrioritized')
    expect(w.get('[data-observation-field="sourceQueue"]').text()).toContain('4.200 s')
    expect(w.get('[data-observation-field="sourceNetwork"]').text()).toContain('1.200 s')
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

it('shows distinct mint and continue evidence with zero offsets and cancellation cause', () => {
  const w = mount(TicketBorrowObservation, { props: { details: {
    candidate_id: 'candidate-traced', batch_id: 'batch-traced', source: {}, target: {},
    probe_queue_ms: 0, cancellation_cause: 'batch_success',
    mint: { duration_ms: 2500, retry_count: 1, attempts: [{ phase: 'mint', number: 2, started_at: '2026-10-12T00:00:00Z', duration_ms: 1000, status: 200, events: { tls_start: 0, completed: 900, eof: 1000 } }] },
    continue: { duration_ms: 7000, retry_count: 0, attempts: [{ phase: 'continue', number: 1, started_at: '2026-10-12T00:00:03Z', duration_ms: 7000, status: 200, events: { first_sse: 2000, completed: 6000 }, error_class: 'timeout', timeout_stage: 'eof', cancellation_cause: 'continue_deadline' }] }
  } } })
  expect(w.get('[data-observation-field="probeQueue"]').text()).toContain('0.000 s')
  expect(w.get('[data-observation-field="cancellationCause"]').text()).toContain('ticketBorrowing.diagnostics.causes.batch_success')
  expect(w.get('[data-probe-phase="mint"]').text()).toContain('2.500 s')
  expect(w.get('[data-probe-phase="mint"]').text()).toContain('+0.000 s')
  expect(w.get('[data-probe-phase="continue"]').text()).toContain('ticketBorrowing.diagnostics.stages.eof')
  expect(w.get('[data-probe-phase="continue"]').text()).toContain('ticketBorrowing.diagnostics.causes.continue_deadline')
})
