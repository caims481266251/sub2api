import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import BorrowEventList from '../BorrowEventList.vue'
import Select from '@/components/common/Select.vue'
import type { BorrowEvent, BorrowSource, BorrowTarget } from '@/api/admin/ticketBorrowing'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : ''), te: (key: string) => !key.endsWith('future_reason') }) }))
const event = (id: number, overrides: Partial<BorrowEvent> = {}): BorrowEvent => ({id, target_id: 11, source_id: 0, source_account_id: 12, route_id: `route-${id}`, stage: id%2 ? 'source' : 'target', reason: id%2 ? 'source_cookie_acquired' : 'target_probe_passed', created_at: '2026-10-10T08:00:00Z', details: {candidate_id: `candidate-${id}`, batch_id: `batch-${id}`, source: {}, target: {}, cookie_age_at_probe_ms: 0}, ...overrides})
const mountList = (events: BorrowEvent[]) => mount(BorrowEventList, {props: {events, sources: [{account_id:12, name:'source A'} as BorrowSource], targets:[{account_id:11, name:'target A'} as BorrowTarget], loading:false, hasOlder:true, visible:true}, global:{stubs:{BaseDialog:{props:['show'],template:'<aside v-if="show"><slot /></aside>'}}}})
async function select(w: ReturnType<typeof mountList>, label: string, value: string | number) { w.findAllComponents(Select).find(s => s.props('ariaLabel') === label)!.vm.$emit('update:modelValue', value); await w.vm.$nextTick() }

describe('borrowing history list', () => {
 it('paginates loaded records and mounts diagnostics only for the opened record', async () => {
  const w=mountList(Array.from({length:200},(_,i)=>event(i+1)))
  expect(w.findAll('[data-event-id]')).toHaveLength(20)
  expect(w.findAll('[data-event-id]')[0].attributes('data-event-id')).toBe('200')
  expect(w.find('[data-candidate-id]').exists()).toBe(false)
  await w.get('[data-event-open="200"]').trigger('click')
  expect(w.findAll('[data-candidate-id]')).toHaveLength(1)
  expect(w.get('[data-observation-field="cookieAgeAtProbe"]').text()).toContain('0.000 s')
  await select(w, 'pagination.perPage', 50)
  expect(w.findAll('[data-event-id]')).toHaveLength(50)
  await w.setProps({visible:false})
  expect(w.find('[data-testid="event-detail"]').exists()).toBe(false)
  w.unmount()
 })
 it('combines exact source and stage filters without conflating legacy and normal account IDs', async () => {
  const w=mountList([event(1),event(2),event(3,{source_account_id:undefined,source_id:12})])
  await select(w, 'ticketBorrowing.source', 'legacy:12')
  expect(w.findAll('[data-event-id]')).toHaveLength(1)
  expect(w.get('[data-event-id="3"]').text()).toContain('ticketBorrowing.legacySource')
  await select(w, 'ticketBorrowing.stage', 'target')
  expect(w.findAll('[data-event-id]')).toHaveLength(0)
  expect(w.text()).toContain('ticketBorrowing.historyPage.noMatches')
  expect(w.find('[data-testid="events-older"]').exists()).toBe(true)
  await select(w, 'ticketBorrowing.source', 'account:12')
  expect(w.find('[data-event-id="2"]').exists()).toBe(true)
  w.unmount()
 })
 it('searches candidate IDs, account names and unknown reason codes; details stay on the opened snapshot', async () => {
  const w=mountList([event(1),event(2,{reason:'future_reason'})])
  await w.get('[data-testid="event-search"]').setValue('candidate-2')
  expect(w.findAll('[data-event-id]')).toHaveLength(1)
  expect(w.get('[data-event-id="2"]').text()).toContain('future_reason')
  await w.get('[data-event-open="2"]').trigger('click')
  await w.setProps({events:[event(3)]})
  expect(w.get('[data-testid="event-detail"]').text()).toContain('route-2')
  await w.get('[data-testid="event-search"]').setValue('source A')
  expect(w.find('[data-event-id="3"]').exists()).toBe(true)
  w.unmount()
 })
 it('keeps filters while new records arrive and disables requests during loading', async () => {
  const w=mountList([event(1),event(2)])
  await select(w, 'ticketBorrowing.stage', 'target')
  await w.setProps({events:[event(4),event(3),event(2),event(1)]})
  expect(w.findAll('[data-event-id]')).toHaveLength(2)
  await w.get('[data-testid="events-refresh"]').trigger('click')
  await w.get('[data-testid="events-older"]').trigger('click')
  expect(w.emitted('refresh')).toHaveLength(1);expect(w.emitted('older')).toHaveLength(1)
  await w.setProps({loading:true})
  expect(w.get('[data-testid="events-refresh"]').attributes('disabled')).toBeDefined()
  expect(w.get('[data-testid="events-older"]').attributes('disabled')).toBeDefined()
  w.unmount()
 })
})
