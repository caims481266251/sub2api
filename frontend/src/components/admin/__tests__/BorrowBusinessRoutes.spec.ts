import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import BorrowBusinessRoutes from '../BorrowBusinessRoutes.vue'
import type { BorrowTarget } from '@/api/admin/ticketBorrowing'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : '') }) }))

describe('business route occupancy', () => {
  it('shows shared account capacity separately from probes and keeps draining streams visible', async () => {
    const now = Date.parse('2026-10-11T04:00:00Z')
    const target = {
      enabled: true, inflight: 2, business_inflight: 7, business_concurrency: 10,
      business_routes: [
        { id: 'a', gateway: 'gateway-a', source_account_id: 13, usable: true, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), inflight: 4 },
        { id: 'b', gateway: 'gateway-b', source_account_id: 17, usable: true, inventory_eligible: false, usable_until: new Date(now + 40000).toISOString(), inflight: 1 },
        { id: 'c', gateway: 'gateway-c', source_account_id: 18, usable: false, inventory_eligible: true, usable_until: new Date(now - 1000).toISOString(), inflight: 2 }
      ]
    } as BorrowTarget
    const w = mount(BorrowBusinessRoutes, { props: { target, now } })
    expect(w.get('[data-testid="business-route-load"]').text()).toContain('{"count":7,"limit":10}')
    expect(w.get('[data-business-route="a"]').text()).toContain('routes.accepting')
    expect(w.get('[data-business-route="b"]').text()).toContain('routes.boundOnly')
    expect(w.get('[data-business-route="c"]').text()).toContain('routes.draining')
    await w.setProps({ now: now + 61000 })
    expect(w.get('[data-business-route="a"]').text()).toContain('routes.draining')
    expect(w.get('[data-business-route="a"] [data-route-inflight]').text()).toBe('4')
  })
})
