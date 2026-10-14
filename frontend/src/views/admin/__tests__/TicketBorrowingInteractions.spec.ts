import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import TicketBorrowingView from '@/views/admin/TicketBorrowingView.vue'
import BorrowSourceModel from '@/components/admin/BorrowSourceModel.vue'
import BorrowProbePolicy from '@/components/admin/BorrowProbePolicy.vue'
import BorrowSourceList from '@/components/admin/BorrowSourceList.vue'
import BorrowTargetList from '@/components/admin/BorrowTargetList.vue'
import BorrowTargetStatistics from '@/components/admin/BorrowTargetStatistics.vue'
import BorrowListPagination from '@/components/admin/BorrowListPagination.vue'
import BorrowEventList from '@/components/admin/BorrowEventList.vue'
const api = vi.hoisted(() => Object.fromEntries(['statistics','saveControls','target','setTarget','saveProbePolicy','saveProbePolicies','sources','targets','settings','events','selection','sourceOptions','saveSelection','restoreSources','saveSettings','refreshTarget','scanTarget'].map(k => [k,vi.fn()])))
const timing = vi.hoisted(() => ({getUsageTiming:vi.fn()}))
const navigation = vi.hoisted(() => ({route:{query:{} as Record<string,string>},replace:vi.fn(),push:vi.fn()}))
vi.mock('@/api/admin/ticketBorrowing', () => ({ticketBorrowing:api}))
vi.mock('@/api/admin/usageTiming',()=>timing)
vi.mock('@/stores/app',()=>({useAppStore:()=>({showSuccess:vi.fn(),showError:vi.fn(),showWarning:vi.fn()})}))
vi.mock('vue-router',async original=>({...await original<typeof import('vue-router')>(),useRoute:()=>navigation.route,useRouter:()=>({replace:navigation.replace,push:navigation.push}),onBeforeRouteLeave:vi.fn()}))
vi.mock('vue-i18n',()=>({useI18n:()=>({t:(key:string,args?:Record<string,unknown>)=>key+(args?JSON.stringify(args):''),te:()=>true})}))
vi.mock('@/components/layout/AppLayout.vue',()=>({default:{template:'<main><slot /></main>'}}))
vi.mock('@/components/admin/operations/SmartOpsNav.vue',()=>({default:{template:'<nav />'}}))
const stubs = {BaseDialog:{props:['show'],template:'<section v-if="show"><button data-close-drawer @click="$emit(\'close\')">Close</button><slot /><slot name="footer" /></section>'},RouterLink:{template:'<a><slot /></a>'}}
const wrappers:any[] = []
const track = (w:any) => { wrappers.push(w);return w }
const page = () => track(mount(TicketBorrowingView,{global:{stubs}}))
const target = (revision=1, concurrency=3):any => ({account_id:11,name:'Target 11',enabled:true,revision:2,acquisition_identity:'source',ready:false,phase:'waiting',reason:'',server_now:new Date().toISOString(),next_attempt_at:new Date().toISOString(),inventory_count:0,business_routes:[],probe_policy:{revision,concurrency,renew_before_seconds:15,use_global:false}})
const source = ():any => ({account_id:12,name:'Source 12',enabled:true,model:'gpt-6-luna',selected_group_ids:[],last_reason:''})
const config = (revision=3):any => ({revision,group_ids:[],account_ids:[12],excluded_account_ids:[],default_model:'gpt-6-luna',rotate_identity:false,use_proxy_pool:true,model_overrides:{}})
const deferred = () => {let resolve!:(value:any)=>void;const promise=new Promise<any>(r=>resolve=r);return {promise,resolve}}
const input = (w:any,testid:string) => w.get(`[data-testid="${testid}"]`)
const val = (w:any,testid:string) => (input(w,testid).element as HTMLInputElement).value
const btn = (w:any,text:string) => w.findAll('button').find((b:any)=>b.text()===text)!
const related = (w:any) => w.findAll('button').filter((b:any)=>b.text()==='ticketBorrowing.management.relatedRecords').at(-1)!
const openTarget = async(w:any)=>{await w.get('[data-tab="targets"]').trigger('click');await w.get('[data-target-open="11"]').trigger('click')}
const openSource = async(w:any)=>{await w.get('[data-tab="sources"]').trigger('click');await w.get('[data-source-open="12"]').trigger('click')}
const refresh = async(w:any)=>{await btn(w,'ticketBorrowing.refresh').trigger('click');await flushPromises()}
beforeEach(()=>{
 vi.resetAllMocks();vi.useFakeTimers();sessionStorage.clear();navigation.route.query={}
 api.sources.mockResolvedValue([source()]);api.targets.mockResolvedValue([target()]);api.events.mockResolvedValue([])
 api.settings.mockResolvedValue({revision:1,running:false,route_pool_size:4,route_cooldown_seconds:120})
 api.selection.mockResolvedValue(config());api.statistics.mockResolvedValue(undefined)
 api.sourceOptions.mockResolvedValue({accounts:[{id:12,name:'Source 12',group_ids:[2],status:'active',schedulable:true,request_eligible:true,is_target:false},{id:11,name:'Target 11',group_ids:[],status:'active',is_target:true}],groups:[{id:2,name:'VIP'}],legacy_count:0,restorable_count:0})
 api.saveSelection.mockImplementation(async(v:any)=>({...v,revision:4}));api.saveSettings.mockImplementation(async(v:any)=>({...v,revision:2}));timing.getUsageTiming.mockResolvedValue({traces:[]})
})
afterEach(()=>{for(const w of wrappers.splice(0))w.unmount();vi.useRealTimers();vi.restoreAllMocks()})
describe('borrowing editor state across navigation',()=>{
 it.each(['records','sources'])('target save completes after navigating to %s and releases all controls',async navigationKind=>{
  const pending=deferred();api.saveProbePolicy.mockReturnValue(pending.promise)
  const w=page();await flushPromises();await openTarget(w)
  await input(w,'probe-concurrency').setValue(5);await input(w,'probe-policy-save').trigger('click')
  expect(api.saveProbePolicy).toHaveBeenCalledOnce();expect(w.findComponent(BorrowTargetList).props('busy')).toBe(true)
  if(navigationKind==='records')await related(w).trigger('click')
  else await btn(input(w,'target-controls'),'ticketBorrowing.management.manageSources').trigger('click')
  expect(w.findComponent(BorrowProbePolicy).exists()).toBe(false)
  pending.resolve(target(2,5));await flushPromises();await w.get('[data-tab="targets"]').trigger('click')
  expect(w.findComponent(BorrowTargetList).props('busy')).toBe(false)
  expect(input(w,'target-batch-settings').attributes('disabled')).toBeUndefined()
  await refresh(w);expect(w.findComponent(BorrowTargetList).props('busy')).toBe(false)
  await openTarget(w);expect(input(w,'acquisition-identity').attributes('disabled')).toBeUndefined()
  expect(val(w,'probe-concurrency')).toBe('5')
  expect(w.findComponent(BorrowProbePolicy).emitted('dirty').at(-1)[0]).toBe(false)
 })
 it('source clean cached editor reopens with the latest model and inheritance',async()=>{
  const w=page();await flushPromises();await openSource(w);expect(val(w,'source-model-input')).toBe('gpt-6-luna')
  await w.get('[data-close-drawer]').trigger('click')
  api.selection.mockResolvedValue({...config(4),model_overrides:{12:'gpt-6-astra'}});api.sources.mockResolvedValue([{...source(),model:'gpt-6-astra'}])
  await refresh(w);await openSource(w)
  const editor=w.findComponent(BorrowSourceModel)
  expect(editor.props('selection').model_overrides[12]).toBe('gpt-6-astra')
  expect(val(w,'source-model-input')).toBe('gpt-6-astra');expect(editor.emitted('dirty').at(-1)[0]).toBe(false)
  expect((input(w,'source-model-default').element as HTMLInputElement).checked).toBe(false)
  expect(editor.text()).not.toContain('ticketBorrowing.remoteChange')
 })
 it('source save followed by records accepts completion and clears the saved draft',async()=>{
  const pending=deferred();api.saveSelection.mockReturnValue(pending.promise)
  const w=page();await flushPromises();await openSource(w)
  await input(w,'source-model-default').setValue(false);await input(w,'source-model-input').setValue('gpt-6-astra');await input(w,'source-model-save').trigger('click')
  expect(api.saveSelection).toHaveBeenCalledOnce();await related(w).trigger('click');expect(w.findComponent(BorrowSourceModel).exists()).toBe(false)
  const saved={...config(4),model_overrides:{12:'gpt-6-astra'}};api.selection.mockResolvedValue(saved);api.sources.mockResolvedValue([{...source(),model:'gpt-6-astra'}]);pending.resolve(saved);await flushPromises()
  await openSource(w);const editor=w.findComponent(BorrowSourceModel)
  expect(editor.props('selection').revision).toBe(4);expect(editor.emitted('dirty').at(-1)[0]).toBe(false)
  await refresh(w);expect(editor.props('selection').revision).toBe(4);expect(editor.text()).not.toContain('ticketBorrowing.remoteChange');expect(input(w,'source-model-save').attributes('disabled')).toBeDefined()
 })
 it('statistics range switch releases timing buttons and ignores the old response',async()=>{
  const pending=deferred();timing.getUsageTiming.mockReturnValue(pending.promise)
  api.statistics.mockResolvedValue({since:new Date().toISOString(),until:new Date().toISOString(),routes:[{route_id:'route-1',gateway:'test',requests:1,completed:1,interrupted:0,unconfirmed:0,latest_usage_id:42}]})
  const w=track(mount(BorrowTargetStatistics,{props:{targetId:11}}));await flushPromises()
  const lastBusiness=()=>btn(w,'ticketBorrowing.statistics.lastBusiness #42')
  await lastBusiness().trigger('click');expect(timing.getUsageTiming).toHaveBeenCalledOnce()
  await btn(w,'ticketBorrowing.historyPage.lastMinutes{"count":60}').trigger('click');await flushPromises()
  pending.resolve({traces:[]});await flushPromises();expect(lastBusiness().attributes('disabled')).toBeUndefined()
  await refresh(w);expect(lastBusiness().attributes('disabled')).toBeUndefined()
 })
 it.each(['source','target'])('%s list clamps a restored page after initial empty data is loaded',async kind=>{
  sessionStorage.setItem(`borrow-management:${kind}:page`,'3')
  const w=kind==='source'?track(mount(BorrowSourceList,{props:{sources:[],groups:[],running:false,busy:false,selectionDirty:false,scopeLoaded:true,selected:[]}})):track(mount(BorrowTargetList,{props:{targets:[],running:false,now:Date.now(),dirtyIds:{},selected:[]}}))
  await w.setProps(kind==='source'?{sources:[source()]}:{targets:[target()]})
  expect(w.findComponent(BorrowListPagination).props('total')).toBe(1);expect(w.findComponent(BorrowListPagination).props('page')).toBe(1)
  expect(w.findAll(kind==='source'?'[data-saved-source-id]':'[data-target-row]')).toHaveLength(1)
  expect(w.text()).toContain('ticketBorrowing.pageInfo{"page":1,"pages":1,"count":1}')
 })
})
describe('borrowing interaction continuity',()=>{
 it('target numeric clear/refill and ordinary polling preserve expanded section and dirty draft',async()=>{
  const visibility=vi.spyOn(document,'visibilityState','get').mockReturnValue('visible')
  const w=page();await flushPromises();await openTarget(w)
  const details=input(w,'target-controls').get('details').element as HTMLDetailsElement;details.open=true
  await input(w,'probe-concurrency').setValue('');await input(w,'probe-concurrency').setValue(5)
  const calls=api.targets.mock.calls.length;await vi.advanceTimersByTimeAsync(5000);await flushPromises()
  expect(api.targets.mock.calls.length).toBeGreaterThan(calls);expect(details.open).toBe(true);expect(val(w,'probe-concurrency')).toBe('5');expect(input(w,'target-controls').get('details').element).toBe(details)
  expect(api.saveProbePolicy).not.toHaveBeenCalled();visibility.mockRestore()
 })
 it('clean target cached editor refreshes on reopen',async()=>{
  const initial=target().probe_policy;const w=track(mount(BorrowProbePolicy,{props:{target:target(2,6),editorState:{draft:initial,baseline:JSON.stringify(initial)}}}))
  expect(val(w,'probe-concurrency')).toBe('6');expect(w.text()).not.toContain('ticketBorrowing.remoteChange')
 })
 it('source grouping stays expanded during input, checkbox changes, and refresh',async()=>{
  const w=page();await flushPromises();await w.get('[data-tab="sources"]').trigger('click');await input(w,'manage-sources').trigger('click')
  const scope=input(w,'source-scope-editor'),details=scope.get('details').element as HTMLDetailsElement;details.open=true
  await scope.get('input[aria-label="ticketBorrowing.searchGroups"]').setValue('VIP');await w.get('[data-group-id="2"]').setValue(true)
  await refresh(w);expect(details.open).toBe(true);expect((w.get('[data-group-id="2"]').element as HTMLInputElement).checked).toBe(true);expect(scope.get('details').element).toBe(details)
 })
 it('source dirty model survives ordinary polling',async()=>{
  vi.spyOn(document,'visibilityState','get').mockReturnValue('visible')
  const w=page();await flushPromises();await openSource(w);await input(w,'source-model-default').setValue(false);await input(w,'source-model-input').setValue('gpt-6-astra')
  const calls=api.sources.mock.calls.length;await vi.advanceTimersByTimeAsync(5000);await flushPromises()
  expect(api.sources.mock.calls.length).toBeGreaterThan(calls);expect(val(w,'source-model-input')).toBe('gpt-6-astra');expect(w.findComponent(BorrowSourceModel).emitted('dirty').at(-1)[0]).toBe(true)
 })
 it('batch numeric clear/refill and failed submission preserve dialog and draft',async()=>{
  api.saveProbePolicies.mockRejectedValue({reason:'borrow_configuration_conflict'})
  const w=page();await flushPromises();await input(w,'target-batch-settings').trigger('click');await input(w,'probe-batch-change-concurrency').setValue(true)
  const form=input(w,'probe-batch-form').element;await input(w,'probe-batch-concurrency').setValue('');await input(w,'probe-batch-concurrency').setValue(7)
  await input(w,'probe-batch-form').trigger('submit');await flushPromises();expect(api.saveProbePolicies).toHaveBeenCalledOnce()
  expect(input(w,'probe-batch-form').element).toBe(form);expect(val(w,'probe-batch-concurrency')).toBe('7');expect(input(w,'probe-batch-form').text()).toContain('borrow_configuration_conflict')
 })
 it('event identifier filter stays expanded when typing and refreshing',async()=>{
  const w=track(mount(BorrowEventList,{props:{events:[],sources:[],targets:[],hasOlder:false,loading:false,visible:true,query:{hide_rotations:true}},global:{stubs}}))
  const details=w.get('details').element as HTMLDetailsElement;details.open=true
  await input(w,'event-route').setValue('123');await input(w,'event-candidate').setValue('456');await w.setProps({events:[],loading:true});await w.setProps({loading:false})
  expect(details.open).toBe(true);expect(val(w,'event-route')).toBe('123');expect(val(w,'event-candidate')).toBe('456');expect(w.get('details').element).toBe(details)
 })
 it('expanded target routes remain expanded on ordinary refresh',async()=>{
  const w=page();await flushPromises();await w.get('[data-expand-routes="11"]').trigger('click');await refresh(w)
  expect(w.get('[data-expand-routes="11"]').attributes('aria-expanded')).toBe('true');expect(w.find('[data-testid="business-routes"]').exists()).toBe(true)
 })
 it('scope view switch preserves search, pending checks, expansion, and applied draft',async()=>{
  const w=page();await flushPromises();await w.get('[data-tab="sources"]').trigger('click');await input(w,'manage-sources').trigger('click')
  await w.get('[data-group-id="2"]').setValue(true);await input(w,'scope-search').setValue('Source');await w.get('[data-account-id="12"]').setValue(true)
  const details=input(w,'source-scope-editor').get('details').element as HTMLDetailsElement;details.open=true
  await btn(w,'ticketBorrowing.management.defaults').trigger('click');await btn(w,'ticketBorrowing.chooseScope').trigger('click')
  expect(val(w,'scope-search')).toBe('Source');expect((w.get('[data-account-id="12"]').element as HTMLInputElement).checked).toBe(true)
  expect((w.get('[data-group-id="2"]').element as HTMLInputElement).checked).toBe(true);expect((input(w,'source-scope-editor').get('details').element as HTMLDetailsElement).open).toBe(true)
 })
})

describe('editing while validation or async state changes', () => {
 it.each([['source-budget',60],['probe-budget',58],['global-probe-concurrency',4],['continue-timeout',8]])('keeps advanced settings open after clearing and refilling %s',async (field,value)=>{
  const w=page();await flushPromises();await input(w,'open-settings').trigger('click')
  const section=input(w,'settings-advanced'),node=section.element as HTMLDetailsElement
  node.open=true;await section.trigger('toggle')
  await input(w,String(field)).setValue('');expect(input(w,String(field)).attributes('aria-invalid')).toBe('true')
  await input(w,String(field)).setValue(value)
  expect(node.open).toBe(true);expect(val(w,String(field))).toBe(String(value))
  await refresh(w);expect(section.element).toBe(node);expect(node.open).toBe(true)
  expect(api.saveSettings).not.toHaveBeenCalled()
 })
 it('allows manual collapse after correcting an intermediate invalid number',async()=>{
  const w=page();await flushPromises();await input(w,'open-settings').trigger('click')
  const section=input(w,'settings-advanced'),node=section.element as HTMLDetailsElement
  expect(node.open).toBe(false)
  await input(w,'probe-budget').setValue(1);expect(node.open).toBe(true)
  await input(w,'probe-budget').setValue(12);expect(node.open).toBe(true)
  node.open=false;await section.trigger('toggle');await refresh(w)
  expect(node.open).toBe(false)
 })
 it.each(['target','source'])('keeps a reopened %s editor pending and accepts the original save completion',async kind=>{
  const pending=deferred()
  if(kind==='target')api.saveProbePolicy.mockReturnValue(pending.promise)
  else api.saveSelection.mockReturnValue(pending.promise)
  const w=page();await flushPromises()
  if(kind==='target'){
   await openTarget(w);await input(w,'probe-concurrency').setValue(5);await input(w,'probe-policy-save').trigger('click')
  }else{
   await openSource(w);await input(w,'source-model-default').setValue(false);await input(w,'source-model-input').setValue('gpt-6-astra');await input(w,'source-model-save').trigger('click')
  }
  await related(w).trigger('click')
  if(kind==='target'){
   await openTarget(w);expect(input(w,'probe-concurrency').attributes('disabled')).toBeDefined()
   await input(w,'probe-policy-save').trigger('click');expect(api.saveProbePolicy).toHaveBeenCalledOnce()
   pending.resolve(target(2,5));await flushPromises()
   expect(val(w,'probe-concurrency')).toBe('5');expect(input(w,'probe-concurrency').attributes('disabled')).toBeUndefined()
   expect(w.findComponent(BorrowProbePolicy).emitted('dirty').at(-1)[0]).toBe(false)
  }else{
   await openSource(w);expect(input(w,'source-model-editor').get('fieldset').attributes('disabled')).toBeDefined()
   await input(w,'source-model-save').trigger('click');expect(api.saveSelection).toHaveBeenCalledOnce()
   pending.resolve({...config(4),model_overrides:{12:'gpt-6-astra'}});await flushPromises()
   expect(val(w,'source-model-input')).toBe('gpt-6-astra');expect(input(w,'source-model-editor').get('fieldset').attributes('disabled')).toBeUndefined()
   expect(w.findComponent(BorrowSourceModel).emitted('dirty').at(-1)[0]).toBe(false)
  }
  expect(w.text()).not.toContain('ticketBorrowing.remoteChange')
  expect(w.findComponent(BorrowTargetList).props('busy')).toBe(false)
 })
 it.each(['target','source'])('retains the %s draft and releases controls after a save fails while closed',async kind=>{
  let reject!:(reason:unknown)=>void
  const pending=new Promise((_resolve,r)=>{reject=r})
  const save=kind==='target'?api.saveProbePolicy:api.saveSelection
  save.mockReturnValueOnce(pending)
  const w=page();await flushPromises()
  if(kind==='target'){
   await openTarget(w);await input(w,'probe-concurrency').setValue(5);await input(w,'probe-policy-save').trigger('click')
  }else{
   await openSource(w);await input(w,'source-model-default').setValue(false);await input(w,'source-model-input').setValue('gpt-6-astra');await input(w,'source-model-save').trigger('click')
  }
  await related(w).trigger('click');reject({reason:'network_error'});await flushPromises()
  expect(w.findComponent(BorrowTargetList).props('busy')).toBe(false)
  if(kind==='target'){
   await openTarget(w);expect(val(w,'probe-concurrency')).toBe('5');expect(input(w,'probe-policy-save').attributes('disabled')).toBeUndefined()
   expect(w.text()).toContain('network_error');save.mockResolvedValue(target(2,5));await input(w,'probe-policy-save').trigger('click')
  }else{
   await openSource(w);expect(val(w,'source-model-input')).toBe('gpt-6-astra');expect(input(w,'source-model-save').attributes('disabled')).toBeUndefined()
   expect(w.text()).toContain('network_error');save.mockResolvedValue({...config(4),model_overrides:{12:'gpt-6-astra'}});await input(w,'source-model-save').trigger('click')
  }
  await flushPromises();expect(save).toHaveBeenCalledTimes(2);expect(w.text()).not.toContain('network_error')
 })
 it('retains a dirty source model and its original revision when reopening after a remote edit',async()=>{
  const w=page();await flushPromises();await openSource(w)
  await input(w,'source-model-default').setValue(false);await input(w,'source-model-input').setValue('my-draft')
  await w.get('[data-close-drawer]').trigger('click')
  api.selection.mockResolvedValue({...config(4),model_overrides:{12:'remote-model'}});await refresh(w);await openSource(w)
  expect(val(w,'source-model-input')).toBe('my-draft');expect(w.findComponent(BorrowSourceModel).text()).toContain('ticketBorrowing.remoteChange')
  expect(input(w,'source-model-save').attributes('disabled')).toBeDefined()
 })
 it.each(['source','target'])('preserves a valid preferred %s page while awaiting initial data',async kind=>{
  sessionStorage.setItem(`borrow-management:${kind}:page`,'3')
  const w=kind==='source'?track(mount(BorrowSourceList,{props:{sources:[],groups:[],running:false,busy:false,selectionDirty:false,selected:[]}})):track(mount(BorrowTargetList,{props:{targets:[],running:false,now:Date.now(),dirtyIds:{}}}))
  await w.setProps(kind==='source'?{sources:Array.from({length:61},(_,i)=>({...source(),account_id:i+1}))}:{targets:Array.from({length:61},(_,i)=>({...target(),account_id:i+1}))})
  expect(w.findComponent(BorrowListPagination).props('page')).toBe(3)
  expect(w.findAll(kind==='source'?'[data-saved-source-id]':'[data-target-row]')).toHaveLength(20)
 })
 it('an old timing response cannot unlock or replace a new timing request after changing range',async()=>{
  const oldRequest=deferred(),newRequest=deferred()
  timing.getUsageTiming.mockReturnValueOnce(oldRequest.promise).mockReturnValueOnce(newRequest.promise)
  api.statistics.mockResolvedValue({since:new Date().toISOString(),until:new Date().toISOString(),routes:[{route_id:'route-1',gateway:'test',latest_usage_id:42}]})
  const w=track(mount(BorrowTargetStatistics,{props:{targetId:11}}));await flushPromises()
  const last=()=>btn(w,'ticketBorrowing.statistics.lastBusiness #42')
  await last().trigger('click');await btn(w,'ticketBorrowing.historyPage.lastMinutes{"count":60}').trigger('click');await flushPromises()
  await last().trigger('click');oldRequest.resolve({traces:[{trace_id:'old-trace',events:{}}]});await flushPromises()
  expect(last().attributes('disabled')).toBeDefined();expect(w.text()).not.toContain('old-trace')
  newRequest.resolve({traces:[{trace_id:'new-trace',events:{}}]});await flushPromises()
  expect(last().attributes('disabled')).toBeUndefined();expect(w.text()).toContain('new-trace')
 })
})
