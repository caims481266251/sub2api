import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import Select from '@/components/common/Select.vue'
import BorrowSourceList from '../BorrowSourceList.vue'
import BorrowTargetList from '../BorrowTargetList.vue'
import type { BorrowSource, BorrowTarget } from '@/api/admin/ticketBorrowing'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : ''), te: () => true }) }))
const source = (id: number): BorrowSource => ({ can_rotate: true, id, account_id: id, name: `source ${id}`, selected_group_ids: [2], explicit: false, unavailable_reason: '', model: 'model', enabled: true, revision: 1, rotate_identity: false, proxy_count: 1, last_reason: '', next_attempt_at: '', use_proxy_pool: true, pool_entry_id: id, pool_endpoint: `proxy-${id}.test:80`, fingerprint_id: `fingerprint-${id}` })
const now = Date.now()
const target = (id: number): BorrowTarget => ({ account_id: id, name: `target ${id}`, enabled: true, revision: 1, ready: true, phase: 'verifying', reason: 'target_probe_passed', expires_at: new Date(now+120000).toISOString(), server_now: new Date(now).toISOString(), next_attempt_at: '', inflight: 3, probe_policy: { revision: 1, concurrency: 3, renew_before_seconds: 15 } })

describe('account management lists', () => {
  it('shows normal-account rate-limit state and reset time for a source', async () => {
    const limited = { ...source(1), enabled: false, unavailable_reason: 'source_rate_limited', rate_limit_reset_at: '2026-11-08T15:47:30Z' }
    const w = mount(BorrowSourceList, { props: { sources: [limited], rotations: [], groups: [], running: true, busy: false, rotationLoaded: true, selectionDirty: false, selected: [] } })
    expect(w.get('[data-saved-source-id="1"] .borrow-badge').text()).toBe('ticketBorrowing.management.rateLimited')
    expect(w.get('[data-testid="source-rate-limit-reset"]').text()).toContain(new Date(limited.rate_limit_reset_at).toLocaleString())
    await w.setProps({ sources: [source(1)] })
    expect(w.get('[data-saved-source-id="1"] .borrow-badge').text()).toBe('ticketBorrowing.management.eligible')
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
    expect(w.get('[data-target-row="26"]').text()).toContain('concurrencyValue{"count":2}')
    w.unmount()
  })
  it('exposes all-account proxy and fingerprint switching in the toolbar across pages and filters', async () => {
    const sources = Array.from({length: 25}, (_, i) => source(i+1))
    sources.push({...source(26), pool_entry_id: undefined}, {...source(27), unavailable_reason: 'source_is_target'}, {...source(28), use_proxy_pool: false})
    const w = mount(BorrowSourceList, { props: { sources, rotations: [], groups: [], running: false, busy: false, rotationLoaded: true, selectionDirty: false, selected: [] } })
    const all = w.get('.borrow-toolbar [data-testid="rotate-all"]')
    expect(all.element.closest('details')).toBeNull()
    await w.get('[data-testid="source-search"]').setValue('source 1')
    await all.trigger('click')
    expect(w.emitted('rotate')?.[0]?.[0]).toEqual(Array.from({length: 25}, (_, i) => i+1))
    await w.setProps({ busy: true })
    expect(all.attributes('disabled')).toBeDefined()
    await w.setProps({ busy: false, rotationLoaded: false })
    expect(all.attributes('disabled')).toBeDefined()
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

describe('source rotation eligibility', () => {
  it('skips unhealthy and draining sources but permits a rate-limited normal account', async () => {
    const sources = [source(1), {...source(2), can_rotate: false, rotation_unavailable_reason: 'source_account_unavailable', unavailable_reason: 'source_account_unavailable'}, {...source(3), enabled: false, unavailable_reason: 'source_rate_limited'}, {...source(4), can_rotate: false, rotation_unavailable_reason: 'borrow_rotation_pending'}]
    const w = mount(BorrowSourceList, { props: { sources, rotations: [], groups: [], running: true, busy: false, rotationLoaded: true, selectionDirty: false, selected: [1, 2] } })
    await w.get('[data-testid="rotate-all"]').trigger('click')
    expect(w.emitted('rotate')?.[0]?.[0]).toEqual([1, 3])
    expect(w.get('[data-rotate-source="2"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-rotate-source="2"]').attributes('title')).toContain('source_account_unavailable')
    expect(w.get('[data-rotate-source="3"]').attributes('disabled')).toBeUndefined()
    await w.get('[data-testid="rotate-selected"]').trigger('click')
    expect(w.emitted('rotate')?.[1]?.[0]).toEqual([1, 2])
    expect(w.get('[data-testid="rotation-excluded"]').text()).toContain('"count":2')
    w.unmount()
  })
})
