import { beforeEach, describe, expect, it, vi } from 'vitest'
const mocks = vi.hoisted(() => ({ put: vi.fn() }))
vi.mock('../../client', () => ({ apiClient: mocks }))
import { ticketBorrowing, defaultBorrowReplenishment } from '../ticketBorrowing'
beforeEach(() => { vi.clearAllMocks(); mocks.put.mockResolvedValue({ data: {} }) })
describe('simplified borrowing API', () => {
 it('sends one admission window and the rolling toggle without retired parameters', async () => {
  await ticketBorrowing.saveSettings({ revision: 4, running: true, admission_window_seconds: 80, ttl_seconds: 230, min_route_remaining_seconds: 120, renew_before_seconds: 180, prewarm_seconds: 60, replenishment: { enabled: true, inventory: 4, hold_seconds: 150, interval_seconds: 10 } })
  const payload = mocks.put.mock.calls[0]![1]
  expect(payload).toMatchObject({ revision: 4, running: true, admission_window_seconds: 80, replenishment: { enabled: true } })
  for (const key of ['ttl_seconds', 'min_route_remaining_seconds', 'renew_before_seconds', 'prewarm_seconds']) expect(payload).not.toHaveProperty(key)
  expect(payload.replenishment).toEqual({ enabled: true })
  expect(defaultBorrowReplenishment().enabled).toBe(false)
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
