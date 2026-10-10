<template>
  <section class="borrow-list card" data-testid="borrow-events">
    <div class="borrow-toolbar"><div><h2 class="font-semibold">{{ t('ticketBorrowing.events') }} <span class="ml-1 text-sm font-normal text-gray-500 dark:text-gray-400">{{ events.length }}</span></h2><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.historyPage.loadedScope', { count: events.length }) }}</p></div><button class="btn btn-secondary btn-sm" :disabled="loading" data-testid="events-refresh" @click="$emit('refresh')">{{ t(loading ? 'ticketBorrowing.loading' : 'ticketBorrowing.historyPage.refresh') }}</button></div>
    <div class="borrow-filters">
      <input v-model="search" class="input borrow-search" :placeholder="t('ticketBorrowing.historyPage.search')" :aria-label="t('ticketBorrowing.historyPage.search')" data-testid="event-search" />
      <Select v-model="stage" class="borrow-select" :options="stageOptions" :aria-label="t('ticketBorrowing.stage')" :searchable="false" data-testid="event-stage" />
      <Select v-model="source" class="borrow-select" :options="sourceOptions" :aria-label="t('ticketBorrowing.source')" searchable data-testid="event-source" />
      <Select v-model="target" class="borrow-select" :options="targetOptions" :aria-label="t('ticketBorrowing.target')" searchable data-testid="event-target" />
      <Select v-model="result" class="borrow-select" :options="resultOptions" :aria-label="t('ticketBorrowing.reason')" searchable data-testid="event-result" />
    </div>
    <div v-if="hasFilters" class="flex flex-wrap items-center justify-between gap-2 border-t border-gray-100 px-4 py-2 text-xs text-gray-500 dark:text-gray-400 dark:border-dark-700"><span>{{ t('ticketBorrowing.historyPage.matches', { count: filtered.length, total: events.length }) }}</span><button class="borrow-link" @click="clear">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <div v-if="rows.length" class="borrow-table-wrap"><table class="borrow-table"><thead><tr><th class="w-[16%]">{{ t('ticketBorrowing.time') }}</th><th class="w-[17%]">{{ t('ticketBorrowing.source') }}</th><th class="w-[17%]">{{ t('ticketBorrowing.target') }}</th><th class="w-[12%]">{{ t('ticketBorrowing.stage') }}</th><th class="w-[29%]">{{ t('ticketBorrowing.reason') }}</th><th class="w-[9%]">{{ t('ticketBorrowing.management.actions') }}</th></tr></thead>
      <tbody><tr v-for="event in rows" :key="event.id" :data-event-id="event.id">
        <td class="borrow-account-cell"><span class="font-medium tabular-nums">{{ date(event.created_at, true) }}</span><span class="borrow-subline">{{ date(event.created_at) }} · #{{ event.id }}</span></td>
        <td :data-label="t('ticketBorrowing.source')"><span class="borrow-truncate" :title="sourceLabel(event)">{{ sourceLabel(event) }}</span><span v-if="event.source_account_id" class="borrow-subline">#{{ event.source_account_id }}</span></td>
        <td :data-label="t('ticketBorrowing.target')"><span class="borrow-truncate" :title="targetLabel(event)">{{ targetLabel(event) }}</span><span v-if="event.target_id" class="borrow-subline">#{{ event.target_id }}</span></td>
        <td :data-label="t('ticketBorrowing.stage')"><span class="borrow-badge borrow-badge-neutral">{{ stageLabel(event.stage) }}</span></td>
        <td :data-label="t('ticketBorrowing.reason')"><span class="borrow-truncate" :title="reason(event.reason)">{{ reason(event.reason) }}</span><span class="borrow-subline font-mono" :title="event.reason">{{ event.reason || '—' }}</span></td>
        <td class="borrow-actions"><button class="borrow-link" :data-event-open="event.id" @click="active = event">{{ t('ticketBorrowing.historyPage.details') }}</button></td>
      </tr></tbody>
    </table></div>
    <div v-else class="borrow-empty"><p>{{ t(loading && !events.length ? 'ticketBorrowing.loading' : events.length ? 'ticketBorrowing.historyPage.noMatches' : 'ticketBorrowing.emptyEvents') }}</p><button v-if="hasFilters" class="borrow-link mt-2" @click="clear">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
    <div class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 px-4 py-3 dark:border-dark-700"><p class="max-w-3xl text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.eventsHint') }}</p><button v-if="hasOlder" class="btn btn-secondary btn-sm" :disabled="loading" data-testid="events-older" @click="$emit('older')">{{ t('ticketBorrowing.older') }}</button></div>
    <BaseDialog :show="active !== null" :title="t('ticketBorrowing.historyPage.detailTitle', { id: active?.id })" width="wide" placement="right" @close="active = null">
      <div v-if="active" class="space-y-5" data-testid="event-detail">
        <dl class="grid gap-4 rounded-xl border border-gray-200 p-4 text-sm dark:border-dark-600 sm:grid-cols-2"><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.time') }}</dt><dd class="mt-1">{{ new Date(active.created_at).toLocaleString() }}</dd></div><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.stage') }}</dt><dd class="mt-1">{{ stageLabel(active.stage) }}</dd></div><div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.source') }}</dt><dd class="mt-1 break-all">{{ sourceLabel(active) }}<span v-if="active.source_account_id"> · #{{ active.source_account_id }}</span></dd></div><div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.target') }}</dt><dd class="mt-1 break-all">{{ targetLabel(active) }}<span v-if="active.target_id"> · #{{ active.target_id }}</span></dd></div></dl>
        <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-900/40"><h3 class="text-sm font-medium">{{ reason(active.reason) }}</h3><dl class="mt-3 space-y-3 text-xs"><div><dt class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.resultCode') }}</dt><dd class="mt-1 break-all font-mono">{{ active.reason || '—' }}</dd></div><div v-if="active.route_id"><dt class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.historyPage.routeId') }}</dt><dd class="mt-1 break-all font-mono">{{ active.route_id }}</dd></div></dl></div>
        <TicketBorrowObservation v-if="active.details" :details="active.details" expanded />
        <p v-else class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.historyPage.noDiagnostics') }}</p>
      </div>
    </BaseDialog>
  </section>
</template>
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import BorrowListPagination from './BorrowListPagination.vue'
import TicketBorrowObservation from './TicketBorrowObservation.vue'
import type { BorrowEvent, BorrowSource, BorrowTarget } from '@/api/admin/ticketBorrowing'
const props = defineProps<{ events: BorrowEvent[]; sources: BorrowSource[]; targets: BorrowTarget[]; hasOlder: boolean; loading: boolean; visible: boolean }>()
defineEmits<{ refresh: []; older: [] }>()
const { t, te } = useI18n()
const search = ref(''), stage = ref(''), source = ref(''), target = ref(0), result = ref(''), page = ref(1), pageSize = ref(20)
const active = ref<BorrowEvent | null>(null)
watch(() => props.visible, visible => { if (!visible) active.value = null })
const sourceNames = computed(() => new Map(props.sources.map(s => [s.account_id, s.name])))
const targetNames = computed(() => new Map(props.targets.map(s => [s.account_id, s.name])))
function sourceKey(event: BorrowEvent) { return event.source_account_id ? `account:${event.source_account_id}` : event.source_id ? `legacy:${event.source_id}` : 'none' }
function sourceLabel(event: BorrowEvent) { return event.source_account_id ? sourceNames.value.get(event.source_account_id) || `#${event.source_account_id}` : event.source_id ? t('ticketBorrowing.legacySource', { id: event.source_id }) : '—' }
function targetLabel(event: BorrowEvent) { return event.target_id ? targetNames.value.get(event.target_id) || `#${event.target_id}` : '—' }
function stageLabel(value: string) { const key = `ticketBorrowing.stages.${value}`; return te(key) ? t(key) : value || t('ticketBorrowing.unknownStage') }
function reason(value: string) { const key = `ticketBorrowing.reasons.${value}`; return value ? te(key) ? t(key) : value : '—' }
function date(value: string, time = false) { const d = new Date(value); return time ? d.toLocaleTimeString() : d.toLocaleDateString() }
const stageOptions = computed(() => [{value: '', label: t('ticketBorrowing.historyPage.allStages')}, ...[...new Set(props.events.map(e => e.stage))].filter(Boolean).sort().map(value => ({value, label: stageLabel(value)}))])
const sourceOptions = computed(() => [{value: '', label: t('ticketBorrowing.historyPage.allSources')}, ...[...new Map(props.events.map(e => [sourceKey(e), {value: sourceKey(e), label: sourceLabel(e) + (e.source_account_id && sourceNames.value.get(e.source_account_id) ? ` #${e.source_account_id}` : '')}])).values()]])
const targetOptions = computed(() => [{value: 0, label: t('ticketBorrowing.historyPage.allTargets')}, ...[...new Map(props.events.filter(e => e.target_id).map(e => [e.target_id, {value: e.target_id, label: targetLabel(e) + (targetNames.value.get(e.target_id) ? ` #${e.target_id}` : '')}])).values()]])
const resultOptions = computed(() => [{value: '', label: t('ticketBorrowing.historyPage.allResults')}, ...[...new Set(props.events.map(e => e.reason))].filter(Boolean).sort().map(value => ({value, label: `${reason(value)} · ${value}`}))])
const hasFilters = computed(() => !!(search.value.trim() || stage.value || source.value || target.value || result.value))
const filtered = computed(() => props.events.filter(e => (!stage.value || e.stage === stage.value) && (!source.value || sourceKey(e) === source.value) && (!target.value || e.target_id === target.value) && (!result.value || e.reason === result.value) && [e.id, e.source_account_id, e.target_id, sourceLabel(e), targetLabel(e), e.reason, reason(e.reason), e.details?.candidate_id, e.route_id, e.details?.gateway].filter(v => v != null).join(' ').toLowerCase().includes(search.value.trim().toLowerCase())).sort((a,b) => b.id-a.id))
const rows = computed(() => filtered.value.slice((page.value-1)*pageSize.value, page.value*pageSize.value))
watch([search, stage, source, target, result, pageSize], () => { page.value = 1 })
watch(() => Math.max(1, Math.ceil(filtered.value.length/pageSize.value)), count => { page.value = Math.min(page.value, count) })
function clear() { search.value = ''; stage.value = ''; source.value = ''; target.value = 0; result.value = '' }
</script>
<style scoped src="./borrowAccountList.css"></style>
