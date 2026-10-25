<template>
  <section class="borrow-list card" data-testid="target-list">
    <div class="borrow-toolbar">
      <div class="flex flex-wrap items-center gap-3"><h2 class="font-semibold">{{ t('ticketBorrowing.targets') }} <span class="ml-1 text-sm font-normal text-gray-500 dark:text-gray-400">{{ targets.length }}</span></h2>
        <div class="flex flex-wrap gap-2 text-xs"><button v-for="item in ['all', 'ready', 'waiting', 'expiring']" :key="item" class="borrow-stat" :class="{ 'borrow-stat-active': filter === item }" :aria-pressed="filter === item" @click="filter = item">{{ t(`ticketBorrowing.management.targetFilters.${item}`) }} <strong class="ml-1 tabular-nums">{{ count(item) }}</strong></button></div>
      </div><div class="flex flex-wrap gap-2"><button class="btn btn-secondary btn-sm" :disabled="busy || !configurableTargets.length" data-testid="target-batch-settings" @click="$emit('batchSettings')">{{ t('ticketBorrowing.probePolicy.bulk.title') }}</button><slot /></div>
    </div>
    <div class="borrow-filters">
      <input v-model="search" class="input borrow-search" :placeholder="t('ticketBorrowing.management.searchTargets')" :aria-label="t('ticketBorrowing.management.searchTargets')" data-testid="target-search" />
      <Select v-model="filter" class="borrow-select" :aria-label="t('ticketBorrowing.management.routeFilter')" data-testid="target-filter" :options="['all', 'ready', 'waiting', 'expiring', 'disabled'].map(value => ({ value, label: t(`ticketBorrowing.management.targetFilters.${value}`) }))" :searchable="false" />
      <Select v-model="phase" class="borrow-select" :aria-label="t('ticketBorrowing.phase')" data-testid="target-phase" :options="['all', 'probing', 'budget', 'upstream', 'paused', 'waiting'].map(value => ({ value, label: t(`ticketBorrowing.management.phases.${value}`) }))" :searchable="false" />
      <Select v-model="sort" class="borrow-select" :aria-label="t('ticketBorrowing.management.sort')" :options="['id', 'name'].map(value => ({ value, label: t(value === 'id' ? 'ticketBorrowing.management.sortId' : 'ticketBorrowing.management.sortName') }))" :searchable="false" />
    </div>
    <div v-if="targets.length" class="borrow-batch">
      <label class="flex items-center gap-2"><input type="checkbox" :checked="allPageSelected" :indeterminate="somePageSelected && !allPageSelected" :disabled="busy || !selectablePage.length" data-testid="select-target-page" @change="selectPage(($event.target as HTMLInputElement).checked)" />{{ t('ticketBorrowing.management.selectPage') }}</label>
      <span>{{ t('ticketBorrowing.management.selectedCount', { count: selected.length }) }}</span>
      <button v-if="selected.length" class="borrow-link" :disabled="busy" @click="$emit('update:selected', [])">{{ t('ticketBorrowing.management.clearSelected') }}</button>
    </div>
    <p v-if="selectionCleared" role="status" class="px-4 py-2 text-xs text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.management.selectionCleared') }}</p>
    <div v-if="rows.length" class="borrow-table-wrap"><table class="borrow-table"><thead><tr><th class="w-[22%]">{{ t('ticketBorrowing.name') }}</th><th class="w-[25%]">{{ t('ticketBorrowing.management.routeStatus') }}</th><th class="w-[15%]">{{ t('ticketBorrowing.management.businessInflight') }}</th><th class="w-[25%]">{{ t('ticketBorrowing.management.probing') }}</th><th class="w-[13%]">{{ t('ticketBorrowing.management.actions') }}</th></tr></thead>
      <tbody><template v-for="target in rows" :key="target.account_id"><tr :data-target-row="target.account_id">
        <td class="borrow-account-cell"><div class="flex min-w-0 items-center gap-2"><input v-if="target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0" type="checkbox" :checked="selected.includes(target.account_id)" :disabled="busy" :aria-label="t('ticketBorrowing.probePolicy.bulk.select', { id: target.account_id })" :data-probe-select="target.account_id" @change="selectOne(target.account_id, ($event.target as HTMLInputElement).checked)" /><button class="borrow-account" :title="target.name" @click="$emit('open', target.account_id)">{{ target.name }}</button></div><span class="borrow-subline">#{{ target.account_id }}<BorrowTag v-if="dirtyIds[target.account_id]" class="ml-2" kind="state" value="unsaved" /></span><div v-if="target.probe_policy" class="mt-1"><BorrowTag kind="state" :value="target.probe_policy.use_global ? 'inherited' : 'overridden'" /></div></td>
        <td :data-label="t('ticketBorrowing.management.routeStatus')">
          <BorrowTag kind="state" :value="borrowTargetAdmissionState(target, now)" data-testid="target-admission" />
          <p v-if="target.business_routes !== undefined" class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400" data-testid="target-route-summary">{{ routeSummary(target) }}</p>
          <button v-if="target.inventory_count !== undefined || target.business_routes?.length" class="borrow-subline tabular-nums borrow-link !whitespace-normal" :aria-expanded="expanded.includes(target.account_id)" :data-expand-routes="target.account_id" @click="expanded = expanded.includes(target.account_id) ? expanded.filter(id => id !== target.account_id) : [...expanded, target.account_id]">{{ t('ticketBorrowing.statusSummary.viewPool', borrowRouteCounts(target, now)) }}</button>
        </td>
        <td :data-label="t('ticketBorrowing.management.businessInflight')">
          <strong class="tabular-nums">{{ t('ticketBorrowing.statusSummary.inflight', { count: target.business_inflight ?? 0 }) }}</strong>
          <p v-if="(target.business_inflight ?? 0) > 0 && !ready(target)" class="mt-1 text-xs leading-5 text-blue-700 dark:text-blue-300" data-testid="target-natural-drain">{{ t('ticketBorrowing.statusSummary.existingContinue') }}</p>
          <span class="borrow-subline !whitespace-normal" data-testid="target-account-load">{{ t('ticketBorrowing.routes.load', { count: borrowAccountCapacity(target).occupied, limit: borrowAccountCapacity(target).limit ?? '—' }) }}</span>
          <span class="borrow-subline !whitespace-normal" data-testid="target-account-remaining">{{ t('ticketBorrowing.routes.sharedRemaining', { count: borrowAccountCapacity(target).remaining ?? '—' }) }}</span>
        </td>
        <td :data-label="t('ticketBorrowing.management.probing')">
          <BorrowTargetStatus :target="target" :running="running" :now="now" :source-budget="sourceBudget" />
          <details v-if="target.reason" class="mt-2 text-xs text-gray-500 dark:text-gray-400" data-testid="target-last-result">
            <summary class="cursor-pointer leading-5">{{ t('ticketBorrowing.statusSummary.lastProbeHistory') }}</summary>
            <p class="mt-1">{{ t('ticketBorrowing.statusSummary.historyHint') }}</p>
            <BorrowTag class="mt-1" :value="target.reason" />
            <span class="borrow-subline">{{ target.last_request_finished_at ? new Date(target.last_request_finished_at).toLocaleTimeString() : '—' }}</span>
          </details>
        </td>
        <td class="borrow-actions"><button class="borrow-link" :data-target-open="target.account_id" @click="$emit('open', target.account_id)">{{ t('ticketBorrowing.management.detailsSettings') }}</button><button class="borrow-link mt-2" :data-target-records="target.account_id" @click="$emit('records', target.account_id)">{{ t('ticketBorrowing.management.relatedRecords') }}</button></td>
      </tr><tr v-if="expanded.includes(target.account_id)"><td colspan="5" class="col-span-2 min-w-0 !block md:!table-cell"><BorrowBusinessRoutes :target="target" :now="now" @records="$emit('records', target.account_id, $event)" /></td></tr></template></tbody>
    </table></div>
    <div v-else class="borrow-empty"><p>{{ t(targets.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.emptyTargets') }}</p><button v-if="targets.length" class="borrow-link mt-2" @click="search = ''; filter = 'all'; phase = 'all'">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, ref, watch } from 'vue'
import { useBorrowPreference } from '@/composables/useBorrowPreference'
import { useI18n } from 'vue-i18n'
import BorrowTargetStatus from './BorrowTargetStatus.vue'
import BorrowBusinessRoutes from './BorrowBusinessRoutes.vue'
import { borrowAccountCapacity, borrowProbeState, borrowSeconds, borrowReady, borrowRouteCounts, borrowTargetAdmissionState, borrowAdmissionDeadline } from '@/composables/borrowStatus'
import type { BorrowTarget, BorrowSettings } from '@/api/admin/ticketBorrowing'
import Select from '@/components/common/Select.vue'
import BorrowListPagination from './BorrowListPagination.vue'
const props = withDefaults(defineProps<{ targets: BorrowTarget[]; running: boolean; now: number; dirtyIds: Record<number, boolean>; selected?: number[]; busy?: boolean; sourceBudget?: BorrowSettings['source_budget'] }>(), { selected: () => [], busy: false })
const emit = defineEmits<{ open: [id: number]; records: [id: number, routeId?: string]; batchSettings: []; 'update:selected': [ids: number[]] }>()
const { t } = useI18n()
const expanded = ref<number[]>([])
const search = useBorrowPreference<string>('target:search', ''), filter = useBorrowPreference<string>('target:filter', 'all'), phase = useBorrowPreference<string>('target:phase', 'all'), sort = useBorrowPreference<string>('target:sort', 'id'), page = useBorrowPreference<number>('target:page', 1), pageSize = useBorrowPreference<number>('target:pageSize', 20), selectionCleared = ref(false)
function seconds(time?: string) { return borrowSeconds(time, props.now) }
function ready(target: BorrowTarget) { return borrowReady(target, props.now) }
function routeSummary(target: BorrowTarget) {
  const counts = borrowRouteCounts(target, props.now)
  const parts = (['accepting', 'busy', 'pending', 'cooling', 'unavailable'] as const)
    .filter(key => counts[key] > 0).map(key => t(`ticketBorrowing.statusSummary.${key}`, { count: counts[key] }))
  return parts.join(' · ') || t('ticketBorrowing.statusSummary.accepting', { count: 0 })
}
function matches(target: BorrowTarget, state: string) { return state === 'ready' ? ready(target) : state === 'waiting' ? target.enabled && !ready(target) : state === 'expiring' ? ready(target) && seconds(borrowAdmissionDeadline(target, props.now)) <= (target.probe_policy?.renew_before_seconds ?? 180) : state === 'disabled' ? !target.enabled : true }
function count(state: string) { return props.targets.filter(target => matches(target, state)).length }
function phaseOf(target: BorrowTarget) { return borrowProbeState(target, props.running, props.now).phase }
const filtered = computed(() => props.targets.filter(target => `${target.account_id} ${target.name} ${target.gateway || ''}`.toLowerCase().includes(search.value.trim().toLowerCase()) && matches(target, filter.value) && (phase.value === 'all' || phaseOf(target) === phase.value)).sort((a,b) => sort.value === 'name' ? a.name.localeCompare(b.name) || a.account_id-b.account_id : a.account_id-b.account_id))
const rows = computed(() => filtered.value.slice((page.value-1)*pageSize.value, page.value*pageSize.value))
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length/pageSize.value)))
watch([search, filter, phase], () => { page.value = 1; selectionCleared.value = props.selected.length > 0; emit('update:selected', []) })
watch([sort, pageSize], () => { page.value = 1 })
watch([pages, () => filtered.value.length], ([n, count]) => {
  // Preserve the preferred page while initial data is loading, then clamp it.
  if (count > 0) page.value = Math.max(1, Math.min(Math.floor(page.value), n))
}, { immediate: true })
const configurableTargets = computed(() => props.targets.filter(target => target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0))
const selectablePage = computed(() => rows.value.filter(target => target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0).map(target => target.account_id))
const allPageSelected = computed(() => selectablePage.value.length > 0 && selectablePage.value.every(id => props.selected.includes(id)))
const somePageSelected = computed(() => selectablePage.value.some(id => props.selected.includes(id)))
function selectPage(checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, ...selectablePage.value])] : props.selected.filter(id => !selectablePage.value.includes(id))); selectionCleared.value = false }
function selectOne(id: number, checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, id])] : props.selected.filter(v => v !== id)); selectionCleared.value = false }
</script>
<style scoped src="./borrowAccountList.css"></style>
