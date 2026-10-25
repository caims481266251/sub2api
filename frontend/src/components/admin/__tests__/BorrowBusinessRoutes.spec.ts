import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import BorrowBusinessRoutes from '../BorrowBusinessRoutes.vue'
import type { BorrowTarget } from '@/api/admin/ticketBorrowing'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : ''), te: () => true }) }))

describe('business route occupancy', () => {
  it('shows one shared budget for multiple routes and no per-route capacity or fallback limit', () => {
    const now = Date.now()
    const target = {
      enabled: true, business_concurrency: 10, business_inflight: 6, inflight: 2,
      business_routes: [
        { id: 'a', gateway: 'gw-a', state: 'normal', usable: true, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), inflight: 5, available_slots: 2 },
        { id: 'b', gateway: 'gw-b', state: 'normal', usable: true, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), inflight: 1, available_slots: 2 }
      ]
    } as BorrowTarget
    const w = mount(BorrowBusinessRoutes, { props: { target, now } })
    expect(w.get('[data-testid="business-route-load"]').text()).toContain('{"count":8,"limit":10}')
    expect(w.findAll('[data-testid="account-shared-capacity"]')).toHaveLength(1)
    expect(w.get('[data-testid="account-shared-capacity"]').text()).toContain('{"business":6,"background":2,"remaining":2}')
    for (const row of w.findAll('[data-business-route]')) {
      expect(row.get('[data-testid="route-admission"] [data-tag-kind="route"]').attributes('data-tag-value')).toBe('accepting')
      expect(row.get('[data-route-available]').text()).toBe('ticketBorrowing.routes.sharedAdmission')
      expect(row.text()).not.toContain('routeLimit')
    }
    expect(w.get('[data-business-route="a"] [data-route-inflight]').text()).toContain('"count":5')
  })

  it('separates full capacity from quality failure and permits early rechecks with a current pass', async () => {
    const now = Date.now()
    const route = { id: 'r', gateway: 'gw', state: 'normal', usable: true, checking: true, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), quality_valid_until: new Date(now + 10000).toISOString(), inflight: 1, available_slots: 2 }
    const target = { enabled: true, business_concurrency: 4, business_inflight: 1, inflight: 1, business_routes: [route] } as BorrowTarget
    const w = mount(BorrowBusinessRoutes, { props: { target, now } })
    expect(w.text()).toContain('routes.accepting')
    expect(w.text()).toContain('statusSummary.refreshingQuality')
    expect(w.get('[data-route-available]').text()).toContain('routes.sharedAdmission')
    expect(w.get('[data-testid="account-shared-capacity"]').text()).toContain('"remaining":2')
    await w.setProps({ target: { ...target, business_routes: [{ ...route, usable: false, available_slots: 0, unavailable_reason: 'account_busy' }] } })
    expect(w.text()).toContain('routes.busy')
    expect(w.text()).toContain('routes.accountBusy')
    expect(w.text()).not.toContain('statusSummary.existingContinue')
    expect(w.text()).not.toContain('routes.accepting')
    expect(w.get('[data-route-count="busy"]').text()).toContain('"count":1')
    await w.setProps({ now: now + 11000 })
    expect(w.get('[data-testid="route-admission"]').text()).toContain('routePool.checking')
    expect(w.text()).toContain('statusSummary.existingContinue')
    expect(w.text()).not.toContain('routes.busy')
  })
  it('blocks the displayed admission when quality expires even before the next refresh', async () => {
    const now = Date.now()
    const target = { enabled: true, business_routes: [{ id: 'quality', gateway: 'gw', state: 'normal', usable: true, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), quality_valid_until: new Date(now + 1000).toISOString(), quality: { status: 'passed', model: 'gpt-6-astra', reasoning_effort: 'medium', answer: '21', checked_at: new Date(now).toISOString() }, inflight: 1 }] } as BorrowTarget
    const w = mount(BorrowBusinessRoutes, { props: { target, now } })
    expect(w.text()).toContain('routes.accepting')
    await w.setProps({ now: now + 2000 })
    expect(w.text()).toContain('quality.pending')
    expect(w.text()).toContain('statusSummary.existingContinue')
    expect(w.text()).not.toContain('routes.accepting')
    expect(w.get('[data-testid="route-details"]').text()).toContain('gpt-6-astra · medium')
  })
  it('excludes pending verification from usable inventory and shows existing streams draining', () => {
    const now = Date.now()
    const target = {
      enabled: true, preferred_route_id: 'pending', business_routes: [
        { id: 'pending', gateway: 'gateway', state: 'pending_probe', checking: true, usable: false, inventory_eligible: true, usable_until: new Date(now + 60000).toISOString(), inflight: 2 }
      ]
    } as BorrowTarget
    const w = mount(BorrowBusinessRoutes, { props: { target, now } })
    const row = w.get('[data-business-route="pending"]')
    expect(row.text()).toContain('statusSummary.checkToResume')
    expect(row.text()).toContain('routePool.checking')
    expect(row.text()).toContain('statusSummary.existingContinue')
    expect(row.text()).not.toContain('routes.accepting')
    expect(w.find('[data-testid="preferred-route"]').exists()).toBe(false)
    expect(w.get('[data-route-count="pending"]').text()).toContain('"count":1')
    expect(w.text()).toContain('"desired":128')
  })
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
    expect(w.get('[data-testid="business-route-load"]').text()).toContain('{"count":9,"limit":10}')
    expect(w.get('[data-testid="account-shared-capacity"]').text()).toContain('{"business":7,"background":2,"remaining":1}')
    expect(w.get('[data-business-route="a"]').text()).toContain('routes.accepting')
    expect(w.get('[data-business-route="b"]').text()).toContain('routes.boundOnly')
    expect(w.get('[data-business-route="c"]').text()).toContain('statusSummary.existingContinue')
    await w.setProps({ now: now + 61000 })
    expect(w.get('[data-business-route="a"]').text()).toContain('statusSummary.existingContinue')
    expect(w.get('[data-business-route="a"] [data-route-inflight]').text()).toBe('ticketBorrowing.statusSummary.inflight{"count":4}')
  })
})


describe('current admission and historical quality', () => {
  const now = Date.parse('2026-10-14T07:14:00Z')
  const until = new Date(now + 3000000).toISOString()
  const base = { id: 'cooling', gateway: 'chat.gateway.unified-96.api.openai.com', source_account_id: 71, state: 'cooling', usable: false, inventory_eligible: true, usable_until: until, inflight: 2, available_slots: 0, cooldown_until: new Date(now + 94000).toISOString(), last_error: 'route_upstream_error', quality: { status: 'passed', model: 'gpt-6-astra', reasoning_effort: 'medium', answer: '21', checked_at: new Date(now - 60000).toISOString() } }
  it('shows cooling, continuing work and an invalidated historical pass without implying admission', async () => {
    const w = mount(BorrowBusinessRoutes, { props: { target: { enabled: true, stored_inventory_count: 1, business_routes: [base] } as BorrowTarget, now } })
    expect(w.get('[data-testid="route-admission"]').text()).toContain('tags.cooling')
    expect(w.get('[data-testid="route-quality-result"] [data-tag-kind="quality"]').attributes('data-tag-tone')).toBe('neutral')
    expect(w.get('[data-testid="route-quality-result"]').text()).toContain('statusSummary.qualityStale')
    expect(w.get('[data-route-inflight]').text()).toContain('"count":2')
    expect(w.get('[data-route-available]').text()).toContain('statusSummary.noNewRequests')
    expect(w.get('[data-testid="route-natural-drain"]').exists()).toBe(true)
    expect(w.text()).not.toContain('2 / 1')
    expect(w.text()).toContain('statusSummary.routeLifetime')
    expect(w.text()).not.toContain('routes.remaining')
    expect(w.get('[data-testid="route-details"]').attributes('open')).toBeUndefined()
    await w.setProps({ now: now + 95000 })
    expect(w.get('[data-testid="route-admission"]').text()).toContain('routePool.checkDue')
    expect(w.find('[data-tag-value="accepting"]').exists()).toBe(false)
    await w.findAll('button').find(button => button.text().includes('relatedRecords'))!.trigger('click')
    expect(w.emitted('records')).toEqual([['cooling']])
  })
  it.each([
    ['29', 'route_quality_failed', 'failed', 'danger'],
    ['', 'route_upstream_error', 'incomplete', 'warning'],
    ['', 'route_quality_timeout', 'incomplete', 'warning'],
    ['21', 'route_quality_model_mismatch', 'model_mismatch', 'warning']
  ])('distinguishes a wrong answer from an incomplete check: %s / %s', (answer, reason, value, tone) => {
    const route = { ...base, quality: { ...base.quality, status: 'failed', answer, reason } }
    const w = mount(BorrowBusinessRoutes, { props: { target: { enabled: true, business_routes: [route] } as BorrowTarget, now } })
    const tag = w.get('[data-testid="route-quality-result"] [data-tag-kind="quality"]')
    expect(tag.attributes('data-tag-value')).toBe(value)
    expect(tag.attributes('data-tag-tone')).toBe(tone)
  })
})
