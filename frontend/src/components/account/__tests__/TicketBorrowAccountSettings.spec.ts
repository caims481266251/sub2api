import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import TicketBorrowAccountSettings from '../TicketBorrowAccountSettings.vue'
const mock=vi.hoisted(()=>({target:vi.fn(),setTarget:vi.fn()}))
vi.mock('@/api/admin/ticketBorrowing',()=>({ticketBorrowing:mock}))
vi.mock('vue-i18n',()=>({useI18n:()=>({t:(key:string,args?:Record<string,unknown>)=>key+(args ? JSON.stringify(args) : ''),te:()=>true})}))
const start=Date.parse('2026-10-10T00:00:00Z')
function status(){return {account_id:11,name:'target',enabled:true,revision:2,ready:true,phase:'verifying',reason:'target_probe_inconclusive',server_now:new Date(start).toISOString(),expires_at:new Date(start+750).toISOString()}}
beforeEach(()=>{vi.useFakeTimers();vi.setSystemTime(start);vi.clearAllMocks();mock.target.mockResolvedValue(status())})
afterEach(()=>vi.useRealTimers())
describe('borrowing target settings',()=>{
 it('keeps subsecond routes usable during replacement verification until actual expiry',async()=>{
  const w=mount(TicketBorrowAccountSettings,{props:{accountId:11}});await flushPromises()
  expect(w.text()).toContain('ticketBorrowing.ready');expect(w.text()).toContain('ticketBorrowing.underSecond')
  await vi.advanceTimersByTimeAsync(750);expect(w.text()).toContain('ticketBorrowing.waiting');w.unmount()
 })
 it('saves enabled state immediately with the authoritative revision',async()=>{
  mock.setTarget.mockResolvedValue({...status(),enabled:false,revision:3,ready:false})
  const w=mount(TicketBorrowAccountSettings,{props:{accountId:11}});await flushPromises();await w.get('input[type="checkbox"]').setValue(false);await flushPromises()
  expect(mock.setTarget).toHaveBeenCalledWith(11,false,2);expect((w.get('input').element as HTMLInputElement).checked).toBe(false);w.unmount()
 })
 it('keeps the confirmed switch after a failed write',async()=>{
  mock.setTarget.mockRejectedValue({reason:'borrow_configuration_conflict'})
  const w=mount(TicketBorrowAccountSettings,{props:{accountId:11}});await flushPromises();await w.get('input').setValue(false);await flushPromises()
  expect(w.get('[role="alert"]').text()).toContain('borrow_configuration_conflict');expect((w.get('input').element as HTMLInputElement).checked).toBe(true);w.unmount()
 })
})
