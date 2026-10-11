<template>
  <section class="borrow-list card" data-testid="borrow-events">
    <div class="borrow-toolbar"><div><h2 class="font-semibold">{{ t('ticketBorrowing.events') }} <span class="ml-1 text-sm font-normal text-gray-500 dark:text-gray-400">{{ events.length }}</span></h2><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.historyPage.loadedScope', { count: events.length }) }}</p></div><button class="btn btn-secondary btn-sm" :disabled="loading" data-testid="events-refresh" @click="showNew">{{ t(loading ? 'ticketBorrowing.loading' : 'ticketBorrowing.historyPage.refresh') }}</button></div>
    <div class="borrow-filters">
      <input v-model="search" class="input borrow-search" :placeholder="t('ticketBorrowing.historyPage.search')" :aria-label="t('ticketBorrowing.historyPage.search')" data-testid="event-search" />
      <Select v-model="stage" class="borrow-select" :options="stageOptions" :aria-label="t('ticketBorrowing.stage')" :searchable="false" data-testid="event-stage" />
      <Select v-model="source" class="borrow-select" :options="sourceOptions" :aria-label="t('ticketBorrowing.source')" searchable data-testid="event-source" />
      <Select v-model="target" class="borrow-select" :options="targetOptions" :aria-label="t('ticketBorrowing.target')" searchable data-testid="event-target" />
      <input v-model="result" class="input borrow-select" :placeholder="t('ticketBorrowing.resultCode')" :aria-label="t('ticketBorrowing.resultCode')" data-testid="event-result" />
    </div>
    <div class="flex flex-wrap gap-3 px-4 py-3 text-xs">
      <label>{{ t('ticketBorrowing.historyPage.from') }}<input type="datetime-local" :value="localDate(query.since)" class="input mt-1" data-testid="event-since" @change="setDate('since', ($event.target as HTMLInputElement).value)" /></label>
      <label>{{ t('ticketBorrowing.historyPage.to') }}<input type="datetime-local" :value="localDate(query.until)" class="input mt-1" data-testid="event-until" @change="setDate('until', ($event.target as HTMLInputElement).value)" /></label>
      <div class="flex flex-wrap items-center gap-2"><button v-for="minutes in [15,60,1440]" :key="minutes" class="borrow-link" @click="query = { ...query, since: new Date(Date.now() - minutes * 60000).toISOString(), until: undefined }">{{ t('ticketBorrowing.historyPage.lastMinutes', { count: minutes }) }}</button></div>
      <label class="flex items-center gap-2"><input v-model="hideRotations" type="checkbox" data-testid="event-hide-rotations" />{{ t('ticketBorrowing.historyPage.hideRotations') }}</label>
      <button class="borrow-link" @click="clear">{{ t('ticketBorrowing.management.resetFilters') }}</button>
    </div>
    <details class="px-4 pb-3 text-xs"><summary class="cursor-pointer">{{ t('ticketBorrowing.historyPage.identifiers') }}</summary><div class="mt-2 flex flex-wrap gap-3"><input v-model="routeId" class="input borrow-select" :placeholder="t('ticketBorrowing.historyPage.routeId')" :aria-label="t('ticketBorrowing.historyPage.routeId')" data-testid="event-route" /><input v-model="candidateId" class="input borrow-select" :placeholder="t('ticketBorrowing.historyPage.candidateId')" :aria-label="t('ticketBorrowing.historyPage.candidateId')" data-testid="event-candidate" /></div></details>
    <button v-if="newCount" class="mx-4 mb-3 text-sm text-primary-600" data-testid="events-new" @click="showNew">{{ t('ticketBorrowing.historyPage.newRecords', { count: newCount }) }}</button>
    <div v-if="rows.length" class="borrow-table-wrap"><table class="borrow-table"><thead><tr><th class="w-[16%]">{{ t('ticketBorrowing.time') }}</th><th class="w-[17%]">{{ t('ticketBorrowing.source') }}</th><th class="w-[17%]">{{ t('ticketBorrowing.target') }}</th><th class="w-[12%]">{{ t('ticketBorrowing.stage') }}</th><th class="w-[29%]">{{ t('ticketBorrowing.reason') }}</th><th class="w-[9%]">{{ t('ticketBorrowing.management.actions') }}</th></tr></thead>
      <tbody><tr v-for="event in rows" :key="event.id" :data-event-id="event.id">
        <td class="borrow-account-cell"><span class="font-medium tabular-nums">{{ date(event.created_at, true) }}</span><span class="borrow-subline">{{ date(event.created_at) }} · #{{ event.id }}</span></td>
        <td :data-label="t('ticketBorrowing.source')"><span class="borrow-truncate" :title="sourceLabel(event)">{{ sourceLabel(event) }}</span><span v-if="event.source_account_id" class="borrow-subline">#{{ event.source_account_id }}</span></td>
        <td :data-label="t('ticketBorrowing.target')"><span class="borrow-truncate" :title="targetLabel(event)">{{ targetLabel(event) }}</span><span v-if="event.target_id" class="borrow-subline">#{{ event.target_id }}</span></td>
        <td :data-label="t('ticketBorrowing.stage')"><BorrowTag kind="stage" :value="event.stage" /></td>
        <td :data-label="t('ticketBorrowing.reason')"><BorrowTag :value="event.reason" /></td>
        <td class="borrow-actions"><button class="borrow-link" :data-event-open="event.id" @click="active = event">{{ t('ticketBorrowing.historyPage.details') }}</button></td>
      </tr></tbody>
    </table></div>
    <div v-else class="borrow-empty"><p>{{ t(loading && !events.length ? 'ticketBorrowing.loading' : hasFilters ? 'ticketBorrowing.historyPage.noMatches' : 'ticketBorrowing.emptyEvents') }}</p><button v-if="hasFilters" class="borrow-link mt-2" @click="clear">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
    <div class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 px-4 py-3 dark:border-dark-700"><p class="max-w-3xl text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.eventsHint') }}</p><button v-if="hasOlder" class="btn btn-secondary btn-sm" :disabled="loading" data-testid="events-older" @click="historyPinned = true; $emit('older')">{{ t('ticketBorrowing.older') }}</button></div>
    <BaseDialog :show="active !== null" :title="t('ticketBorrowing.historyPage.detailTitle', { id: active?.id })" width="wide" placement="right" @close="active = null">
      <div v-if="active" class="space-y-5" data-testid="event-detail">
        <dl class="grid gap-4 rounded-xl border border-gray-200 p-4 text-sm dark:border-dark-600 sm:grid-cols-2"><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.time') }}</dt><dd class="mt-1">{{ new Date(active.created_at).toLocaleString() }}</dd></div><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.stage') }}</dt><dd class="mt-1"><BorrowTag kind="stage" :value="active.stage" /></dd></div><div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.source') }}</dt><dd class="mt-1 break-all">{{ sourceLabel(active) }}<span v-if="active.source_account_id"> · #{{ active.source_account_id }}</span></dd></div><div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.target') }}</dt><dd class="mt-1 break-all">{{ targetLabel(active) }}<span v-if="active.target_id"> · #{{ active.target_id }}</span></dd></div></dl>
        <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900/40"><BorrowTag :value="active.reason" detail /><dl class="mt-3 space-y-3 text-xs"><div><dt class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.resultCode') }}</dt><dd class="mt-1 break-all font-mono">{{ active.reason || '—' }}</dd></div><div v-if="active.route_id"><dt class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.historyPage.routeId') }}</dt><dd class="mt-1 break-all font-mono">{{ active.route_id }}</dd></div></dl></div>
        <div class="flex flex-wrap gap-3 text-sm"><button v-if="active.route_id" class="borrow-link" @click="query = { ...query, route_id: active.route_id }; active = null">{{ t('ticketBorrowing.historyPage.sameRoute') }}</button><button v-if="active.details?.candidate_id" class="borrow-link" @click="query = { ...query, candidate_id: active.details.candidate_id }; active = null">{{ t('ticketBorrowing.historyPage.sameCandidate') }}</button><button v-if="active.route_id || active.details?.candidate_id" class="borrow-link" @click="copyIdentifiers">{{ t('ticketBorrowing.historyPage.copyIdentifiers') }}</button><span v-if="copyMessage" role="status">{{ copyMessage }}</span></div>
        <BorrowReasonHint :code="active.reason" />
        <TicketBorrowObservation v-if="active.details" :details="active.details" />
        <p v-else class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.historyPage.noDiagnostics') }}</p>
      </div>
    </BaseDialog>
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import BorrowListPagination from './BorrowListPagination.vue'
import TicketBorrowObservation from './TicketBorrowObservation.vue'
import BorrowReasonHint from './BorrowReasonHint.vue'
import type { BorrowEventQuery, BorrowEvent, BorrowSource, BorrowTarget } from '@/api/admin/ticketBorrowing'
const props = defineProps<{ events: BorrowEvent[]; sources: BorrowSource[]; targets: BorrowTarget[]; hasOlder: boolean; loading: boolean; visible: boolean; newCount?: number }>()
const emit = defineEmits<{ refresh: []; older: []; readingHistory: [value: boolean] }>()
const { t, te } = useI18n()
const query = defineModel<BorrowEventQuery>('query', { default: () => ({ hide_rotations: true }) })
function field<K extends keyof BorrowEventQuery>(key: K, fallback: NonNullable<BorrowEventQuery[K]>) {
 return computed({ get: () => query.value[key] ?? fallback, set: value => { query.value = { ...query.value, [key]: value || undefined } } })
}
const search = field('search', ''), stage = field('stage', ''), target = field('target_id', 0), result = field('reason', '')
const source = computed({ get: () => query.value.source_account_id ? `account:${query.value.source_account_id}` : query.value.source_id ? `legacy:${query.value.source_id}` : '', set: (value: string) => { const [kind, id] = value.split(':'); query.value = { ...query.value, source_account_id: kind === 'account' ? Number(id) : undefined, source_id: kind === 'legacy' ? Number(id) : undefined } } })
const routeId = field('route_id', ''), candidateId = field('candidate_id', '')
const hideRotations = computed({ get: () => query.value.hide_rotations !== false, set: value => { query.value = { ...query.value, hide_rotations: value } } })
const page = ref(1), pageSize = ref(20), historyPinned = ref(false), copyMessage = ref('')
const active = ref<BorrowEvent | null>(null)
watch(() => props.visible, visible => { if (!visible) active.value = null })
watch(() => page.value > 1 || historyPinned.value || active.value !== null, value => emit('readingHistory', value), { immediate: true })
watch(active, () => { copyMessage.value = '' })
function localDate(value?: string) { if (!value || !Number.isFinite(Date.parse(value))) return ''; const d = new Date(value); return new Date(d.getTime() - d.getTimezoneOffset()*60000).toISOString().slice(0,16) }
function setDate(key: 'since' | 'until', value: string) { query.value = { ...query.value, [key]: value ? new Date(value).toISOString() : undefined } }
async function copyIdentifiers() {
 try { await navigator.clipboard.writeText([active.value?.route_id, active.value?.details?.candidate_id].filter(Boolean).join('\n')); copyMessage.value = t('ticketBorrowing.historyPage.copied') }
 catch { copyMessage.value = t('ticketBorrowing.historyPage.copyFailed') }
}
function showNew() { page.value = 1; historyPinned.value = false; active.value = null; emit('refresh') }
const sourceNames = computed(() => new Map(props.sources.map(s => [s.account_id, s.name])))
const targetNames = computed(() => new Map(props.targets.map(s => [s.account_id, s.name])))
function sourceLabel(event: BorrowEvent) { return event.source_account_id ? sourceNames.value.get(event.source_account_id) || `#${event.source_account_id}` : event.source_id ? t('ticketBorrowing.legacySource', { id: event.source_id }) : '—' }
function targetLabel(event: BorrowEvent) { return event.target_id ? targetNames.value.get(event.target_id) || `#${event.target_id}` : '—' }
function stageLabel(value: string) { const key = `ticketBorrowing.stages.${value}`; return te(key) ? t(key) : value || t('ticketBorrowing.unknownStage') }
function date(value: string, time = false) { const d = new Date(value); return time ? d.toLocaleTimeString() : d.toLocaleDateString() }
const stageOptions = computed(() => [{value: '', label: t('ticketBorrowing.historyPage.allStages')}, ...[...new Set(['source', 'candidate', 'target', 'route', 'rotation', 'diagnostic', ...props.events.map(e => e.stage)])].filter(Boolean).sort().map(value => ({value, label: stageLabel(value)}))])
const sourceOptions = computed(() => [{value: '', label: t('ticketBorrowing.historyPage.allSources')}, ...[...new Set([...props.sources.map(s => s.account_id), ...props.events.map(e => e.source_account_id || 0), query.value.source_account_id ?? 0])].filter(Boolean).sort((a,b) => a-b).map(id => ({value: `account:${id}`, label: `${sourceNames.value.get(id) || ''} #${id}`})), ...[...new Set(props.events.filter(e => !e.source_account_id && e.source_id).map(e => e.source_id).concat(query.value.source_id ? [query.value.source_id] : []))].map(id => ({value: `legacy:${id}`, label: t('ticketBorrowing.legacySource', { id })}))])
const targetOptions = computed(() => [{value: 0, label: t('ticketBorrowing.historyPage.allTargets')}, ...[...new Set([...props.targets.map(t => t.account_id), ...props.events.map(e => e.target_id), Number(target.value)])].filter(Boolean).sort((a,b) => a-b).map(value => ({value, label: `${targetNames.value.get(value) || ''} #${value}`}))])
const hasFilters = computed(() => Object.entries(query.value).some(([key, value]) => key !== 'hide_rotations' && !!value))
const filtered = computed(() => [...props.events].sort((a,b) => b.id-a.id))
const rows = computed(() => filtered.value.slice((page.value-1)*pageSize.value, page.value*pageSize.value))
watch([query, pageSize], () => { page.value = 1; historyPinned.value = false }, { deep: true })
watch(() => Math.max(1, Math.ceil(filtered.value.length/pageSize.value)), count => { page.value = Math.min(page.value, count) })
function clear() { query.value = { hide_rotations: true } }
</script>
<style scoped src="./borrowAccountList.css"></style>
