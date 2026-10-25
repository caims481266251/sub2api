import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BorrowTargetStatistics from '../BorrowTargetStatistics.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => values ? `${key} ${JSON.stringify(values)}` : key, te: () => true }) }))
const { statistics } = vi.hoisted(() => ({ statistics: vi.fn() }))
vi.mock('@/api/admin/ticketBorrowing', () => ({ ticketBorrowing: { statistics } }))
vi.mock('@/api/admin/usageTiming', () => ({ getUsageTiming: vi.fn() }))

describe('probe phase statistics', () => {
 it('keeps phase request attempts separate from candidate verdicts and shows diagnostic coverage', async () => {
  statistics.mockResolvedValue({ target_id: 1, since: '2026-10-12T06:00:00Z', until: '2026-10-12T06:10:00Z', routes: [],
   mint: { requests: 4, diagnostic_samples: 3, retries: 1, average_ms: 1500, timeouts: 0 },
   continue: { requests: 2, diagnostic_samples: 2, retries: 0, average_ms: 7000, timeouts: 1, timeout_stages: { eof: 1 }, cancellations: { batch_success: 1 } }
  })
  const w = mount(BorrowTargetStatistics, { props: { targetId: 1 } }); await flushPromises()
  expect(w.get('[data-phase-statistics="mint"]').text()).toContain('"requests":4')
  expect(w.get('[data-phase-statistics="mint"]').text()).toContain('"samples":3')
  expect(w.get('[data-phase-statistics="continue"]').text()).toContain('ticketBorrowing.diagnostics.causes.batch_success')
  expect(w.get('[data-phase-statistics="continue"]').text()).toContain('ticketBorrowing.diagnostics.stages.eof')
 })
})
