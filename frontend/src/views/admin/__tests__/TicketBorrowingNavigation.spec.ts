import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import TicketBorrowingView from '../TicketBorrowingView.vue'
const api = vi.hoisted(() => ({ sources: vi.fn(), targets: vi.fn(), settings: vi.fn(), events: vi.fn(), selection: vi.fn(), sourceOptions: vi.fn(), rotations: vi.fn(), statistics: vi.fn() }))
vi.mock('@/api/admin/ticketBorrowing', () => ({ ticketBorrowing: api, defaultBorrowReplenishment: () => ({ enabled: false, inventory: 2, interval_seconds: 10, hold_seconds: 150, maintenance_interval_seconds: 30 }) }))
vi.mock('@/api/admin/usageTiming', () => ({ getUsageTiming: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn(), showWarning: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
beforeEach(() => {
 sessionStorage.clear(); vi.clearAllMocks(); vi.useFakeTimers()
 api.sources.mockResolvedValue([]); api.events.mockResolvedValue([]); api.rotations.mockResolvedValue([])
 api.targets.mockResolvedValue([{ account_id: 63, name: 'target', enabled: true, revision: 1, ready: false, server_now: new Date().toISOString(), reason: '', phase: '', next_attempt_at: '' }])
 api.settings.mockResolvedValue({ revision: 1, running: false, ttl_seconds: 230, prewarm_seconds: 0 })
 api.selection.mockResolvedValue({ revision: 1, group_ids: [], account_ids: [], excluded_account_ids: [], default_model: 'model', model_overrides: {}, rotate_identity: false, use_proxy_pool: true })
 api.sourceOptions.mockResolvedValue({ accounts: [], groups: [], legacy_count: 0, restorable_count: 0 })
})
afterEach(() => vi.useRealTimers())

describe('borrow management URL history', () => {
 it('preserves deep links when changing tabs and restores account filters on browser back', async () => {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/admin/ticket-borrowing', component: TicketBorrowingView }] })
  await router.push('/admin/ticket-borrowing'); await router.isReady()
  const w = mount({ template: '<router-view />' }, { global: { plugins: [router], stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show"><slot /></section>' } } } })
  await flushPromises()
  await w.get('[data-target-records="63"]').trigger('click'); await flushPromises(); await vi.advanceTimersByTimeAsync(260)
  expect(router.currentRoute.value.query).toMatchObject({ tab: 'events', target: '63' })
  expect(api.events).toHaveBeenLastCalledWith(expect.objectContaining({ target_id: 63 }))
  await w.get('[data-tab="sources"]').trigger('click'); await flushPromises()
  expect(router.currentRoute.value.query).toMatchObject({ tab: 'sources', target: '63' })
  router.back(); await flushPromises(); await vi.advanceTimersByTimeAsync(260)
  expect(router.currentRoute.value.query).toMatchObject({ tab: 'events', target: '63' })
  expect(w.get('[data-tab="events"]').attributes('aria-current')).toBe('page')
  router.back(); await flushPromises(); await vi.advanceTimersByTimeAsync(260)
  expect(router.currentRoute.value.query).toEqual({ tab: 'targets' })
  expect(w.get('[data-tab="targets"]').attributes('aria-current')).toBe('page')
  w.unmount()
 })
})
