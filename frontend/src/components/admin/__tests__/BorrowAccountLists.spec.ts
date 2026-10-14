import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import Select from '@/components/common/Select.vue'
import BorrowSourceList from '../BorrowSourceList.vue'
import BorrowTargetList from '../BorrowTargetList.vue'
import type { BorrowSource, BorrowTarget } from '@/api/admin/ticketBorrowing'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : ''), te: () => true }) }))
const source = (id: number): BorrowSource => ({ can_rotate: true, id, account_id: id, name: `source ${id}`, selected_group_ids: [2], explicit: false, unavailable_reason: '', model: 'model', enabled: true, revision: 1, rotate_identity: false, proxy_count: 1, last_reason: '', next_attempt_at: '', use_proxy_pool: true, pool_entry_id: id, pool_endpoint: `proxy-${id}.test:80`, fingerprint_id: `fingerprint-${id}` })
const now = Date.now()
const target = (id: number): BorrowTarget => ({ account_id: id, name: `target ${id}`, enabled: true, revision: 1, ready: true, phase: 'verifying', reason: 'target_probe_passed', expires_at: new Date(now+120000).toISOString(), server_now: new Date(now).toISOString(), next_attempt_at: '', inflight: 3, probe_policy: { revision: 1, concurrency: 3, renew_before_seconds: 15 } })

beforeEach(() => sessionStorage.clear())

describe('account management lists', () => {
  it('explains continuing requests when all stored routes are cooling and keeps old probe results secondary', async () => {
    const current = { ...target(63), ready: false, business_inflight: 2, inventory_count: 0, stored_inventory_count: 1, reason: 'borrow_acquisition_paused',
      business_routes: [{ id: 'r', gateway: 'gw', source_account_id: 71, state: 'cooling', usable: false, inventory_eligible: true, inflight: 2, usable_until: new Date(now + 300000).toISOString() }] } as BorrowTarget
    const w = mount(BorrowTargetList, { props: { targets: [current], running: true, now, dirtyIds: {} } })
    expect(w.get('[data-testid="target-admission"]').attributes('data-tag-value')).toBe('noRoute')
    expect(w.get('[data-testid="target-route-summary"]').text()).toContain('statusSummary.cooling{"count":1}')
    expect(w.get('[data-testid="target-natural-drain"]').text()).toContain('statusSummary.existingContinue')
    expect(w.get('[data-testid="target-probe-state"] [data-tag-kind="phase"]').attributes('data-tag-value')).toBe('probing')
    const history = w.get('[data-testid="target-last-result"]')
    expect(history.attributes('open')).toBeUndefined()
    expect(history.get('summary').text()).toContain('statusSummary.lastProbeHistory')
    await w.get('[data-expand-routes="63"]').trigger('click')
    expect(w.find('[data-testid="business-routes"]').exists()).toBe(true)
    await w.setProps({ targets: [{ ...current, business_routes: [{ ...current.business_routes![0], state: 'normal', unavailable_reason: 'route_busy', dispatch_limit: 2 }] }] })
    expect(w.get('[data-testid="target-admission"]').attributes('data-tag-value')).toBe('busy')
    w.unmount()
  })
  it('keeps source cooldown, previous acquisition and paused automatic rotation independent', () => {
    const current = { ...source(1), enabled: false, unavailable_reason: 'source_account_cooling', last_reason: 'source_cookie_acquired' }
    const w = mount(BorrowSourceList, { props: { sources: [current], rotations: [{ account_id: 1, automatic: true, interval_seconds: 30 } as import('@/api/admin/ticketBorrowing').BorrowRotationState], groups: [], running: false, busy: false, rotationLoaded: true, selectionDirty: false, selected: [] } })
    expect(w.get('[data-testid="source-eligibility"]').text()).toBe('ticketBorrowing.tags.cooling')
    expect(w.get('[data-testid="source-last-result"]').text()).toBe('ticketBorrowing.tags.candidateAcquired')
    expect(w.text()).toContain('ticketBorrowing.simplified.randomEnvironment')
    expect(w.find('[data-tag-value="rotationActive"]').exists()).toBe(false)
    w.unmount()
  })

  it('does not use historical success to claim an expired route is accepting', () => {
    const current = { ...target(1), inflight: 0, usable_until: new Date(now - 1000).toISOString() }
    const w = mount(BorrowTargetList, { props: { targets: [current], running: false, now, dirtyIds: {} } })
    expect(w.find('[data-tag-value="noRoute"]').exists()).toBe(true)
    expect(w.find('[data-tag-kind="phase"][data-tag-value="paused"]').exists()).toBe(true)
    expect(w.find('[data-tag-value="target_probe_passed"]').exists()).toBe(true)
    expect(w.find('[data-tag-value="ready"]').exists()).toBe(false)
    w.unmount()
  })

  it('shows normal-account rate-limit state and reset time for a source', async () => {
    const limited = { ...source(1), enabled: false, unavailable_reason: 'source_rate_limited', rate_limit_reset_at: '2026-11-08T15:47:30Z' }
    const w = mount(BorrowSourceList, { props: { sources: [limited], rotations: [], groups: [], running: true, busy: false, rotationLoaded: true, selectionDirty: false, selected: [] } })
    expect(w.get('[data-saved-source-id="1"] [data-testid="source-eligibility"] .borrow-tag').text()).toBe('ticketBorrowing.tags.rateLimited')
    expect(w.get('[data-testid="source-rate-limit-reset"]').text()).toContain(new Date(limited.rate_limit_reset_at).toLocaleString())
    await w.setProps({ sources: [source(1)] })
    expect(w.get('[data-saved-source-id="1"] [data-testid="source-eligibility"] .borrow-tag').text()).toBe('ticketBorrowing.management.eligible')
    expect(w.find('[data-testid="source-rate-limit-reset"]').exists()).toBe(false)
    w.unmount()
  })

  it('stops showing a route as ready at its admission deadline before cookie expiry', async () => {
    const route = { ...target(1), expires_at: new Date(now+121000).toISOString(), usable_until: new Date(now+1000).toISOString() }
    const w = mount(BorrowTargetList, { props: { targets: [route], running: true, now, dirtyIds: {} } })
    expect(w.get('[data-target-row="1"]').text()).toContain('ticketBorrowing.ready')
    await w.setProps({ now: now + 1000 })
    expect(w.get('[data-target-row="1"]').text()).toContain('ticketBorrowing.management.noRoute')
    expect(w.get('[data-target-row="1"]').text()).not.toContain('ticketBorrowing.remaining')
    w.unmount()
  })
  it('selects target accounts across pages and clears selection when filters change', async () => {
    const targets = [...Array.from({length: 25}, (_, i) => target(i+1)), {...target(26), probe_policy: undefined}]
    const w = mount(BorrowTargetList, { props: { targets, running: true, now, dirtyIds: {}, selected: [24] } })
    await w.get('[data-testid="select-target-page"]').setValue(true)
    expect(w.emitted('update:selected')?.at(-1)?.[0]).toEqual([24, ...Array.from({length: 20}, (_, i) => i+1)])
    await w.get('[data-testid="target-search"]').setValue('target 26')
    expect(w.emitted('update:selected')?.at(-1)?.[0]).toEqual([])
    expect(w.find('[data-probe-select="26"]').exists()).toBe(false)
    expect(w.text()).toContain('ticketBorrowing.management.selectionCleared')
    expect(w.get('[data-target-row="26"]').text()).toContain('ticketBorrowing.management.accountConcurrency')
    w.unmount()
  })
  it('explains automatic random environments without manual rotation controls', () => {
    const w = mount(BorrowSourceList, { props: { sources: [source(1)], groups: [], running: false, busy: false, selectionDirty: false, selected: [] } })
    expect(w.text()).toContain('ticketBorrowing.simplified.randomEnvironment')
    expect(w.find('[data-testid="rotate-all"]').exists()).toBe(false)
    expect(w.find('[data-testid="rotation-batch-settings"]').exists()).toBe(false)
    w.unmount()
  })
  it('limits 1000 sources to one page and clears batch selection on filter changes', async () => {
    const w = mount(BorrowSourceList, { props: { sources: Array.from({length: 1000}, (_, i) => source(i+1)), rotations: [], groups: [], running: true, busy: false, rotationLoaded: true, selectionDirty: false, selected: [800] } })
    expect(w.findAll('[data-saved-source-id]')).toHaveLength(20)
    expect(w.find('[data-rotation-source]').exists()).toBe(false)
    await w.get('[data-testid="select-source-page"]').setValue(true)
    expect(w.emitted('update:selected')?.at(-1)?.[0]).toEqual([800, ...Array.from({length: 20}, (_, i) => i+1)])
    await w.get('[data-testid="source-search"]').setValue('proxy-999.test')
    expect(w.findAll('[data-saved-source-id]')).toHaveLength(1)
    expect(w.find('[data-saved-source-id="999"]').exists()).toBe(true)
    expect(w.emitted('update:selected')?.at(-1)?.[0]).toEqual([])
    expect(w.text()).toContain('ticketBorrowing.management.selectionCleared')
    w.unmount()
  })
  it('paginates targets, preserves stable ID order on refresh, and exposes no hidden editors', async () => {
    const targets = Array.from({length: 200}, (_, i) => target(200-i))
    const w = mount(BorrowTargetList, { props: { targets, running: true, now, dirtyIds: {} } })
    expect(w.findAll('[data-target-row]')).toHaveLength(20)
    expect(w.find('[data-probe-policy]').exists()).toBe(false)
    expect(w.findAll('[data-target-row]')[0].attributes('data-target-row')).toBe('1')
    w.findAllComponents(Select).find(c => c.props('ariaLabel') === 'pagination.perPage')!.vm.$emit('update:modelValue', 50); await w.vm.$nextTick()
    expect(w.findAll('[data-target-row]')).toHaveLength(50)
    await w.findAll('button').find(b => b.text() === 'pagination.next')!.trigger('click')
    expect(w.findAll('[data-target-row]')[0].attributes('data-target-row')).toBe('51')
    await w.setProps({ targets: [...targets].reverse() })
    expect(w.findAll('[data-target-row]')[0].attributes('data-target-row')).toBe('51')
    await w.get('[data-testid="target-search"]').setValue('target 199')
    expect(w.findAll('[data-target-row]')).toHaveLength(1)
    expect(w.find('[data-target-row="199"]').exists()).toBe(true)
    w.unmount()
  })
  it('separates route readiness, renewal, background pause and expired routes', async () => {
    const a = { ...target(1), expires_at: new Date(now+10000).toISOString() }
    const b = { ...target(2), expires_at: new Date(now-1000).toISOString() }
    const w = mount(BorrowTargetList, { props: { targets: [a,b], running: true, now, dirtyIds: {} } })
    expect(w.get('[data-target-row="1"]').text()).toContain('ticketBorrowing.ready')
    expect(w.get('[data-target-row="1"]').text()).toContain('ticketBorrowing.management.phases.probing')
    expect(w.get('[data-target-row="2"]').text()).toContain('ticketBorrowing.management.noRoute')
    w.findAllComponents(Select).find(c => c.props('ariaLabel') === 'ticketBorrowing.management.routeFilter')!.vm.$emit('update:modelValue', 'expiring'); await w.vm.$nextTick()
    expect(w.findAll('[data-target-row]')).toHaveLength(1)
    await w.setProps({ running: false, targets: [{...a, inflight: 0}, b] })
    expect(w.get('[data-target-row="1"]').text()).toContain('ticketBorrowing.ready')
    expect(w.get('[data-target-row="1"]').text()).toContain('ticketBorrowing.management.phases.paused')
    await w.setProps({ now: now+11000 })
    expect(w.findAll('[data-target-row]')).toHaveLength(0)
    w.unmount()
  })
})
