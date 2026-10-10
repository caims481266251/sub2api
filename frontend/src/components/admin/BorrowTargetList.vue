<template>
  <section class="borrow-list card" data-testid="target-list">
    <div class="borrow-toolbar">
      <div class="flex flex-wrap items-center gap-3"><h2 class="font-semibold">{{ t('ticketBorrowing.targets') }} <span class="ml-1 text-sm font-normal text-gray-500">{{ targets.length }}</span></h2>
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
    <div v-if="rows.length" class="borrow-table-wrap"><table class="borrow-table"><thead><tr><th class="w-[19%]">{{ t('ticketBorrowing.name') }}</th><th class="w-[15%]">{{ t('ticketBorrowing.management.routeStatus') }}</th><th class="w-[15%]">{{ t('ticketBorrowing.management.probing') }}</th><th class="w-[18%]">{{ t('ticketBorrowing.gateway') }}</th><th class="w-[12%]">{{ t('ticketBorrowing.probePolicy.title') }}</th><th class="w-[13%]">{{ t('ticketBorrowing.reason') }}</th><th class="w-[8%]">{{ t('ticketBorrowing.management.actions') }}</th></tr></thead>
      <tbody><tr v-for="target in rows" :key="target.account_id" :data-target-row="target.account_id">
        <td class="borrow-account-cell"><div class="flex min-w-0 items-center gap-2"><input v-if="target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0" type="checkbox" :checked="selected.includes(target.account_id)" :disabled="busy" :aria-label="t('ticketBorrowing.probePolicy.bulk.select', { id: target.account_id })" :data-probe-select="target.account_id" @change="selectOne(target.account_id, ($event.target as HTMLInputElement).checked)" /><button class="borrow-account" :title="target.name" @click="$emit('open', target.account_id)">{{ target.name }}</button></div><span class="borrow-subline">#{{ target.account_id }}<span v-if="dirtyIds[target.account_id]" class="ml-2 text-amber-600">{{ t('ticketBorrowing.unsaved') }}</span></span></td>
        <td :data-label="t('ticketBorrowing.management.routeStatus')"><span class="borrow-badge" :class="ready(target) ? 'borrow-badge-good' : target.enabled ? 'borrow-badge-warn' : 'borrow-badge-neutral'">{{ t(!target.enabled ? 'ticketBorrowing.disabled' : ready(target) ? 'ticketBorrowing.ready' : 'ticketBorrowing.management.noRoute') }}</span><span v-if="ready(target)" class="borrow-subline tabular-nums">{{ t('ticketBorrowing.remaining', { time: t('ticketBorrowing.seconds', { count: seconds(target.expires_at) }) }) }}</span></td>
        <td :data-label="t('ticketBorrowing.management.probing')"><span>{{ phaseLabel(target) }}</span><span class="borrow-subline">{{ target.inflight ? t('ticketBorrowing.management.probeInflight', { count: target.inflight }) : reason(target.phase) }}</span></td>
        <td :data-label="t('ticketBorrowing.gateway')"><span class="borrow-truncate font-mono" :title="ready(target) ? target.gateway : ''">{{ ready(target) ? target.gateway : '—' }}</span></td>
        <td :data-label="t('ticketBorrowing.probePolicy.title')"><span>{{ t('ticketBorrowing.management.concurrencyValue', { count: target.probe_policy?.concurrency ?? 2 }) }}</span><span class="borrow-subline">{{ t('ticketBorrowing.management.renewValue', { count: target.probe_policy?.renew_before_seconds ?? 180 }) }}</span><span v-if="target.probe_policy" class="borrow-subline">{{ t(target.probe_policy.use_global ? 'ticketBorrowing.probePolicy.inherited' : 'ticketBorrowing.probePolicy.overridden') }}</span></td>
        <td :data-label="t('ticketBorrowing.reason')"><span class="borrow-truncate" :title="reason(target.reason)">{{ reason(target.reason) }}</span><span class="borrow-subline">{{ target.last_request_finished_at ? new Date(target.last_request_finished_at).toLocaleTimeString() : '—' }}</span></td>
        <td class="borrow-actions"><button class="borrow-link" :data-target-open="target.account_id" @click="$emit('open', target.account_id)">{{ t('ticketBorrowing.management.detailsSettings') }}</button></td>
      </tr></tbody>
    </table></div>
    <div v-else class="borrow-empty"><p>{{ t(targets.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.emptyTargets') }}</p><button v-if="targets.length" class="borrow-link mt-2" @click="search = ''; filter = 'all'; phase = 'all'">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
  </section>
</template>
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowTarget } from '@/api/admin/ticketBorrowing'
import Select from '@/components/common/Select.vue'
import BorrowListPagination from './BorrowListPagination.vue'
const props = withDefaults(defineProps<{ targets: BorrowTarget[]; running: boolean; now: number; dirtyIds: Record<number, boolean>; selected?: number[]; busy?: boolean }>(), { selected: () => [], busy: false })
const emit = defineEmits<{ open: [id: number]; batchSettings: []; 'update:selected': [ids: number[]] }>()
const { t, te } = useI18n()
const search = ref(''), filter = ref('all'), phase = ref('all'), sort = ref('id'), page = ref(1), pageSize = ref(20), selectionCleared = ref(false)
function seconds(time?: string) { return time ? Math.max(0, Math.ceil((Date.parse(time) - props.now) / 1000)) : 0 }
function ready(target: BorrowTarget) { return target.enabled && target.ready && seconds(target.usable_until ?? target.expires_at) > 0 }
function matches(target: BorrowTarget, state: string) { return state === 'ready' ? ready(target) : state === 'waiting' ? target.enabled && !ready(target) : state === 'expiring' ? ready(target) && seconds(target.expires_at) <= (target.probe_policy?.renew_before_seconds ?? 180) : state === 'disabled' ? !target.enabled : true }
function count(state: string) { return props.targets.filter(target => matches(target, state)).length }
function phaseOf(target: BorrowTarget) { return !target.enabled || !props.running ? 'paused' : (target.inflight ?? 0) > 0 ? 'probing' : seconds(target.upstream_wait_until) > 0 ? 'upstream' : seconds(target.budget_wait_until) > 0 ? 'budget' : 'waiting' }
function phaseLabel(target: BorrowTarget) { return !props.running && (target.inflight ?? 0) > 0 ? t('ticketBorrowing.draining') : t(`ticketBorrowing.management.phases.${phaseOf(target)}`) }
function reason(code: string) { const key = `ticketBorrowing.reasons.${code}`; return code ? te(key) ? t(key) : t('ticketBorrowing.unknownResult') : '—' }
const filtered = computed(() => props.targets.filter(target => `${target.account_id} ${target.name} ${target.gateway || ''}`.toLowerCase().includes(search.value.trim().toLowerCase()) && matches(target, filter.value) && (phase.value === 'all' || phaseOf(target) === phase.value)).sort((a,b) => sort.value === 'name' ? a.name.localeCompare(b.name) || a.account_id-b.account_id : a.account_id-b.account_id))
const rows = computed(() => filtered.value.slice((page.value-1)*pageSize.value, page.value*pageSize.value))
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length/pageSize.value)))
watch([search, filter, phase], () => { page.value = 1; selectionCleared.value = props.selected.length > 0; emit('update:selected', []) })
watch([sort, pageSize], () => { page.value = 1 })
watch(pages, n => { page.value = Math.min(page.value, n) })
const configurableTargets = computed(() => props.targets.filter(target => target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0))
const selectablePage = computed(() => rows.value.filter(target => target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0).map(target => target.account_id))
const allPageSelected = computed(() => selectablePage.value.length > 0 && selectablePage.value.every(id => props.selected.includes(id)))
const somePageSelected = computed(() => selectablePage.value.some(id => props.selected.includes(id)))
function selectPage(checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, ...selectablePage.value])] : props.selected.filter(id => !selectablePage.value.includes(id))); selectionCleared.value = false }
function selectOne(id: number, checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, id])] : props.selected.filter(v => v !== id)); selectionCleared.value = false }
</script>
<style scoped src="./borrowAccountList.css"></style>
