import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BorrowCandidatePool from '../BorrowCandidatePool.vue'
import Select from '@/components/common/Select.vue'
import BorrowListPagination from '../BorrowListPagination.vue'
import type { BorrowCandidatePoolPage } from '@/api/admin/ticketBorrowing'

const api = vi.hoisted(() => ({ candidatePool: vi.fn(), candidatePoolDetail: vi.fn(), saveCandidatePoolConfig: vi.fn() }))
vi.mock('@/api/admin/ticketBorrowing', () => ({ ticketBorrowing: api }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : ''), te: () => true }) }))

const time = '2026-10-12T15:00:00Z'
function snapshot(overrides: Partial<BorrowCandidatePoolPage> = {}): BorrowCandidatePoolPage {
  return {
    summary: { capacity: 64, revision: 1, running: true, occupied: 3, acquiring: 1, queued: 1, claimed: 1, waiting_targets: 1, demand_slots: 2 },
    total: 3, page: 1, page_size: 25, server_now: time,
    items: [{ id: 'candidate-queued', state: 'queued', source_id: 1, source_account_id: 13, source_name: 'Source 13', target_id: 0, target_name: '', gateway: 'gateway-a', created_at: time, enqueued_at: time, admission_until: '2026-10-12T15:02:00Z', queue_until: '2026-10-12T15:00:15Z', reason: '' }],
    ...overrides
  }
}
function render() {
  return mount(BorrowCandidatePool, { props: { visible: true, sources: [], targets: [] }, global: { stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show"><slot /></section>' } } } })
}
beforeEach(() => {
  vi.useFakeTimers(); vi.setSystemTime(new Date(time)); vi.resetAllMocks()
  api.candidatePool.mockResolvedValue(snapshot())
})
afterEach(() => vi.useRealTimers())

describe('candidate pool management', () => {
  it('shows global occupancy, separate queue expiry, and linked candidate details', async () => {
    const w = render()
    try {
      await flushPromises()
      expect(w.get('[data-testid="pool-occupancy"]').text()).toBe('3 / 64')
      expect(api.candidatePool).toHaveBeenCalledWith(expect.objectContaining({ state: 'active', page: 1 }))
      expect(w.get('[data-pool-candidate="candidate-queued"]').text()).toContain('queueRemaining{"seconds":15}')
      api.candidatePoolDetail.mockResolvedValue({ ...snapshot().items[0], state: 'finished', reason: 'target_probe_passed', target_id: 63, route_id: 'route-a', details: { candidate_id: 'candidate-queued', batch_id: '', source: {}, target: {} }, server_now: time })
      await w.get('[data-pool-detail="candidate-queued"]').trigger('click'); await flushPromises()
      expect(api.candidatePoolDetail).toHaveBeenCalledWith('candidate-queued')
      const recordButton = w.findAll('button').filter(b => b.text().includes('management.relatedRecords')).at(-1)!
      await recordButton.trigger('click')
      expect(w.emitted('records')).toEqual([[{ candidate_id: 'candidate-queued' }]])
      expect(w.find('[data-observation-field="candidateID"]').exists()).toBe(false)
    } finally { w.unmount() }
  })

  it('keeps a capacity draft across refreshes and detects a concurrent administrator edit', async () => {
    const w = render()
    try {
      await flushPromises()
      await w.get('[data-testid="pool-capacity"]').setValue(128)
      const next = snapshot(); next.summary.capacity = 32; next.summary.revision = 2
      api.candidatePool.mockResolvedValue(next)
      await vi.advanceTimersByTimeAsync(5000); await flushPromises()
      expect((w.get('[data-testid="pool-capacity"]').element as HTMLInputElement).value).toBe('128')
      expect(w.get('[data-testid="pool-occupancy"]').text()).toBe('3 / 32')
      expect(w.text()).toContain('candidatePool.conflict')
      expect(w.get('[data-testid="pool-capacity-save"]').attributes('disabled')).toBeDefined()
      await w.findAll('button').find(b => b.text().includes('candidatePool.useSaved'))!.trigger('click')
      expect((w.get('[data-testid="pool-capacity"]').element as HTMLInputElement).value).toBe('32')
      await w.get('[data-testid="pool-capacity"]').setValue(48)
      api.saveCandidatePoolConfig.mockResolvedValue({ capacity: 48, revision: 3 })
      const applied = snapshot(); applied.summary.capacity = 48; applied.summary.revision = 3
      api.candidatePool.mockResolvedValue(applied)
      await w.get('form').trigger('submit'); await flushPromises()
      expect(api.saveCandidatePoolConfig).toHaveBeenCalledWith({ capacity: 48, revision: 2 })
      expect(w.text()).toContain('candidatePool.saved')
      expect(w.emitted('dirty')?.at(-1)).toEqual([false])
    } finally { w.unmount() }
  })

  it('allows a smaller capacity while displaying existing occupancy and validates integers', async () => {
    const w = render()
    try {
      await flushPromises()
      for (const value of [0, -1, 1.5, 1025]) {
        await w.get('[data-testid="pool-capacity"]').setValue(value)
        expect(w.get('[data-testid="pool-capacity-save"]').attributes('disabled')).toBeDefined()
      }
      await w.get('[data-testid="pool-capacity"]').setValue(1)
      api.saveCandidatePoolConfig.mockResolvedValue({ capacity: 1, revision: 2 })
      const applied = snapshot(); applied.summary.capacity = 1; applied.summary.revision = 2
      api.candidatePool.mockResolvedValue(applied)
      await w.get('form').trigger('submit'); await flushPromises()
      expect(w.get('[data-testid="pool-occupancy"]').text()).toBe('3 / 1')
      expect(w.get('[data-testid="pool-over-capacity"]').text()).toContain('candidatePool.draining')
    } finally { w.unmount() }
  })

  it('does not poll hidden tabs or move history pages; older responses cannot overwrite new filters', async () => {
    const w = render()
    try {
      await flushPromises()
      await w.setProps({ visible: false })
      await vi.advanceTimersByTimeAsync(10000)
      expect(api.candidatePool).toHaveBeenCalledTimes(1)
      await w.setProps({ visible: true }); await flushPromises()
      let resolve!: (value: BorrowCandidatePoolPage) => void
      api.candidatePool.mockReturnValueOnce(new Promise(r => { resolve = r }))
      await w.get('[data-testid="pool-refresh"]').trigger('click')
      const finished = snapshot({ items: [], total: 0 })
      api.candidatePool.mockResolvedValue(finished)
      w.findAllComponents(Select)[0]!.vm.$emit('update:modelValue', 'finished')
      await flushPromises()
      resolve(snapshot()); await flushPromises()
      expect(w.find('[data-pool-candidate]').exists()).toBe(false)
      const calls = api.candidatePool.mock.calls.length
      await vi.advanceTimersByTimeAsync(10000)
      expect(api.candidatePool).toHaveBeenCalledTimes(calls)
      w.findComponent(BorrowListPagination).vm.$emit('update:page', 2)
      await flushPromises()
      expect(api.candidatePool).toHaveBeenLastCalledWith(expect.objectContaining({ state: 'finished', page: 2 }))
    } finally { w.unmount() }
  })

  it('retains a rejected save and marks a failed refresh as stale instead of displaying zero', async () => {
    const w = render()
    try {
      await flushPromises()
      await w.get('[data-testid="pool-capacity"]').setValue(128)
      api.saveCandidatePoolConfig.mockRejectedValue({ response: { status: 409 } })
      const next = snapshot(); next.summary.capacity = 32; next.summary.revision = 2
      api.candidatePool.mockResolvedValue(next)
      await w.get('form').trigger('submit'); await flushPromises()
      expect((w.get('[data-testid="pool-capacity"]').element as HTMLInputElement).value).toBe('128')
      expect(w.text()).toContain('candidatePool.conflict')
      api.candidatePool.mockRejectedValue(new Error('offline'))
      await w.get('[data-testid="pool-refresh"]').trigger('click'); await flushPromises()
      expect(w.get('[data-testid="pool-occupancy"]').text()).toBe('3 / 32')
      expect(w.text()).toContain('candidatePool.loadFailed')
    } finally { w.unmount() }
  })
})
