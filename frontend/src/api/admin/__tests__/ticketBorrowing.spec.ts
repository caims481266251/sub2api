import { beforeEach, describe, expect, it, vi } from 'vitest'
const mocks = vi.hoisted(() => ({ put: vi.fn() }))
vi.mock('../../client', () => ({ apiClient: mocks }))
import { ticketBorrowing } from '../ticketBorrowing'
beforeEach(() => { vi.clearAllMocks(); mocks.put.mockResolvedValue({ data: {} }) })
describe('simplified borrowing API', () => {
 it('persists the global quality model, effort and periodic interval', async () => {
  const route_quality = { model: 'gpt-6-astra', reasoning_effort: 'medium', interval_seconds: 30 }
  await ticketBorrowing.saveSettings({ running: true, revision: 7, route_quality })
  expect(mocks.put.mock.calls[0]![1]).toMatchObject({ route_quality })
 })
 it('does not resend a retired route threshold from an older server response', async () => {
  const legacy = { revision: 1, running: true, ttl_seconds: 80, prewarm_seconds: 0, route_429_threshold: 2 }
  await ticketBorrowing.saveSettings(legacy)
  expect(mocks.put.mock.calls[0]![1]).not.toHaveProperty('route_429_threshold')
 })
 it('sends pool settings and drops all retired parameters from stale drafts', async () => {
  const draft = { revision: 4, running: true, route_pool_size: 4, route_cooldown_seconds: 120, admission_window_seconds: 80, ttl_seconds: 230, min_route_remaining_seconds: 120, renew_before_seconds: 180, prewarm_seconds: 60, replenishment: { enabled: true, inventory: 4, hold_seconds: 150, interval_seconds: 10 } }
  await ticketBorrowing.saveSettings(draft)
  const payload = mocks.put.mock.calls[0]![1]
  expect(payload).toMatchObject({ revision: 4, running: true, route_pool_size: 4, route_cooldown_seconds: 120 })
  for (const key of ['admission_window_seconds', 'ttl_seconds', 'min_route_remaining_seconds', 'renew_before_seconds', 'prewarm_seconds', 'replenishment']) expect(payload).not.toHaveProperty(key)
 })
 it('sends the independent acquisition revision when changing identity', async () => {
  await ticketBorrowing.setTarget(63, true, 2, 'self', 3)
  expect(mocks.put).toHaveBeenCalledWith('/admin/accounts/63/ticket-borrowing', { enabled: true, revision: 2, acquisition_identity: 'self', acquisition_revision: 3 })
 })
 it('keeps concurrency independent from automatically computed renewal', async () => {
  await ticketBorrowing.saveProbePolicy(63, { revision: 4, concurrency: 3, renew_before_seconds: 180, use_global: false })
  expect(mocks.put.mock.calls[0]![1]).toEqual({ revision: 4, concurrency: 3, use_global: false })
 })
})
