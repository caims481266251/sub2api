import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import TicketBorrowingView from '../TicketBorrowingView.vue'
const api = vi.hoisted(() => ({ sources: vi.fn(), targets: vi.fn(), settings: vi.fn(), events: vi.fn(), selection: vi.fn(), sourceOptions: vi.fn(), rotations: vi.fn(), statistics: vi.fn(), saveProbePolicy: vi.fn(), saveSelection: vi.fn() }))
vi.mock('@/api/admin/ticketBorrowing', () => ({ ticketBorrowing: api }))
vi.mock('@/api/admin/usageTiming', () => ({ getUsageTiming: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn(), showWarning: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
beforeEach(() => {
 sessionStorage.clear(); vi.clearAllMocks(); vi.useFakeTimers()
 api.sources.mockResolvedValue([]); api.events.mockResolvedValue([]); api.rotations.mockResolvedValue([])
 api.targets.mockResolvedValue([{ account_id: 63, name: 'target', enabled: true, revision: 1, ready: false, server_now: new Date().toISOString(), reason: '', phase: '', next_attempt_at: '' }])
 api.settings.mockResolvedValue({ revision: 1, running: false,route_pool_size:4,route_cooldown_seconds:120 })
 api.selection.mockResolvedValue({ revision: 1, group_ids: [], account_ids: [], excluded_account_ids: [], default_model: 'model', model_overrides: {}, rotate_identity: false, use_proxy_pool: true })
 api.sourceOptions.mockResolvedValue({ accounts: [], groups: [], legacy_count: 0, restorable_count: 0 })
})
afterEach(() => vi.useRealTimers())

describe('borrow management URL history', () => {
 it.each(['target', 'source'])('keeps a pending %s save on the page until its result has been accepted', async kind => {
  const target = { account_id: 63, name: 'target', enabled: true, revision: 1, ready: false, server_now: new Date().toISOString(), reason: '', phase: '', next_attempt_at: '', probe_policy: { revision: 1, concurrency: 2, renew_before_seconds: 0, use_global: false } }
  const selection = { revision: 1, group_ids: [], account_ids: [12], excluded_account_ids: [], default_model: 'model', model_overrides: {}, rotate_identity: false, use_proxy_pool: true }
  api.targets.mockResolvedValue([target])
  api.sources.mockResolvedValue([{ account_id: 12, name: 'source', enabled: true, model: 'model', selected_group_ids: [] }])
  api.selection.mockResolvedValue(selection)
  let resolve!: (value: unknown) => void
  const pending = new Promise(r => { resolve = r })
  if (kind === 'target') api.saveProbePolicy.mockReturnValueOnce(pending)
  else api.saveSelection.mockReturnValueOnce(pending)
  const router = createRouter({ history: createMemoryHistory(), routes: [
   { path: '/admin/ticket-borrowing', component: TicketBorrowingView },
   { path: '/elsewhere', component: { template: '<p>Other page</p>' } }
  ] })
  await router.push('/admin/ticket-borrowing'); await router.isReady()
  const w = mount({ template: '<router-view />' }, { global: { plugins: [router], stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show"><slot /></section>' } } } })
  try {
   await flushPromises()
   if (kind === 'target') {
    await w.get('[data-target-open="63"]').trigger('click')
    await w.get('[data-testid="probe-concurrency"]').setValue(5)
    await w.get('[data-testid="probe-policy-save"]').trigger('click')
   } else {
    await w.get('[data-tab="sources"]').trigger('click')
    await w.get('[data-source-open="12"]').trigger('click')
    await w.get('[data-testid="source-model-default"]').setValue(false)
    await w.get('[data-testid="source-model-input"]').setValue('new-model')
    await w.get('[data-testid="source-model-save"]').trigger('click')
   }
   await router.push('/elsewhere'); await flushPromises()
   expect(router.currentRoute.value.path).toBe('/admin/ticket-borrowing')
   resolve(kind === 'target' ? { ...target, probe_policy: { ...target.probe_policy, revision: 2, concurrency: 5 } } : { ...selection, revision: 2, model_overrides: { 12: 'new-model' } })
   await flushPromises()
   await router.push('/elsewhere'); await flushPromises()
   expect(router.currentRoute.value.path).toBe('/elsewhere')
  } finally { w.unmount() }
 })
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
