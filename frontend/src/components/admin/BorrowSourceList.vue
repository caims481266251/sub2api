<template>
  <section class="borrow-list card" data-testid="source-list">
    <div class="borrow-toolbar">
      <div><h2 class="font-semibold">{{ t('ticketBorrowing.sources') }} <span class="ml-1 text-sm font-normal text-gray-500 dark:text-gray-400">{{ sources.length }}</span></h2></div>
      <div class="flex flex-wrap gap-2">
<button class="btn btn-secondary btn-sm" data-testid="source-defaults" @click="$emit('defaults')">{{ t('ticketBorrowing.management.defaults') }}</button><button class="btn btn-primary btn-sm" data-testid="manage-sources" @click="$emit('manage')">{{ t('ticketBorrowing.management.manageSources') }}</button></div>
    </div>
    <div class="borrow-filters">
      <input v-model="search" class="input borrow-search" :placeholder="t('ticketBorrowing.management.searchSources')" :aria-label="t('ticketBorrowing.management.searchSources')" data-testid="source-search" />
      <Select v-model="filter" class="borrow-select" :aria-label="t('ticketBorrowing.filterSources')" data-testid="source-filter" :options="filters.map(value => ({ value, label: t(`ticketBorrowing.management.sourceFilters.${value}`) }))" :searchable="false" />
      <Select v-model="group" class="borrow-select" :aria-label="t('ticketBorrowing.selectGroups')" :options="[{ value: 0, label: t('ticketBorrowing.management.allGroups') }, ...groups.map(g => ({ value: g.id, label: g.name }))]" :searchable="true" />
      <Select v-model="sort" class="borrow-select" :aria-label="t('ticketBorrowing.management.sort')" :options="['id', 'name'].map(value => ({ value, label: t(value === 'id' ? 'ticketBorrowing.management.sortId' : 'ticketBorrowing.management.sortName') }))" :searchable="false" />
    </div>
    <div v-if="sources.some(s => s.account_id > 0)" class="borrow-batch">
      <label class="flex items-center gap-2"><input type="checkbox" :checked="allPageSelected" :indeterminate="somePageSelected && !allPageSelected" :disabled="!selectablePage.length || busy" data-testid="select-source-page" @change="selectPage(($event.target as HTMLInputElement).checked)" />{{ t('ticketBorrowing.management.selectPage') }}</label>
      <button class="borrow-link" :disabled="busy || !filtered.length" data-testid="select-source-matching" @click="selectIds(filtered.filter(s => s.account_id > 0).map(s => s.account_id))">{{ t('ticketBorrowing.scope.selectMatching', { count: filtered.filter(s => s.account_id > 0).length }) }}</button>
      <span>{{ t('ticketBorrowing.scope.checked', { count: selected.length }) }}</span>
      <button class="borrow-link" :disabled="busy || !scopeLoaded || !selected.length" data-testid="remove-selected-sources" @click="$emit('remove', [...selected])">{{ t('ticketBorrowing.scope.remove', { count: selected.length }) }}</button>
      <button v-if="selected.length" class="borrow-link" @click="$emit('update:selected', [])">{{ t('ticketBorrowing.scope.clearChecks') }}</button>
    </div>
    <p v-if="selectionCleared" role="status" class="px-4 py-2 text-xs text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.management.selectionCleared') }}</p>
    <p v-if="defaultModel" class="px-4 pb-2 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.management.sourceModelDefault', { model: defaultModel }) }}</p>
    <div v-if="rows.length" class="borrow-table-wrap">
      <table class="borrow-table">
        <thead><tr><th class="w-[23%]">{{ t('ticketBorrowing.name') }}</th><th class="w-[15%]">{{ t('ticketBorrowing.eligibility') }}</th><th class="w-[26%]">{{ t('ticketBorrowing.simplified.probeEnvironment') }}</th><th class="w-[22%]">{{ t('ticketBorrowing.replenishment.lastResult') }}</th><th class="w-[14%]">{{ t('ticketBorrowing.management.actions') }}</th></tr></thead>
        <tbody><tr v-for="source in rows" :key="source.account_id" :data-saved-source-id="source.account_id">
          <td class="borrow-account-cell"><div class="flex min-w-0 items-center gap-2"><input v-if="source.account_id > 0" :checked="selected.includes(source.account_id)" type="checkbox" :disabled="busy" :aria-label="t('ticketBorrowing.scope.checkAccount', { id: source.account_id })" :data-rotation-select="source.account_id" @change="selectOne(source.account_id, ($event.target as HTMLInputElement).checked)" /><button class="borrow-account" :title="source.name" @click="$emit('open', source.account_id)">{{ source.name || `#${source.account_id}` }}</button></div><span class="borrow-subline">#{{ source.account_id }} · {{ origin(source) }}</span><div v-if="modelOverrides?.[source.account_id]" class="mt-1"><BorrowTag kind="state" value="modelOverride" /><span class="borrow-subline" :title="source.model">{{ source.model }}</span></div></td>
          <td :data-label="t('ticketBorrowing.eligibility')"><BorrowTag :kind="source.unavailable_reason ? 'reason' : 'state'" :value="source.unavailable_reason || (source.enabled ? 'eligible' : 'unavailable')" data-testid="source-eligibility" /><span v-if="source.unavailable_reason === 'source_rate_limited' && source.rate_limit_reset_at" class="borrow-subline" data-testid="source-rate-limit-reset">{{ t('ticketBorrowing.management.resetsAt', { time: formatReset(source.rate_limit_reset_at) }) }}</span></td>
          <td :data-label="t('ticketBorrowing.simplified.probeEnvironment')"><span class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.simplified.randomEnvironment') }}</span></td>
          <td :data-label="t('ticketBorrowing.replenishment.lastResult')"><BorrowTag :value="source.last_reason" data-testid="source-last-result" /></td>
          <td class="borrow-actions"><button class="borrow-link" :data-source-open="source.account_id" @click="$emit('open', source.account_id)">{{ t('ticketBorrowing.management.detailsSettings') }}</button><button class="borrow-link mt-2" :data-source-records="source.account_id" @click="$emit('records', source.account_id)">{{ t('ticketBorrowing.management.relatedRecords') }}</button></td>
        </tr></tbody>
      </table>
    </div>
    <div v-else class="borrow-empty"><p>{{ t(sources.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.selectSourceHint') }}</p><button v-if="sources.length" class="borrow-link mt-2" @click="search = ''; filter = 'all'; group = 0">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, ref, watch } from 'vue'
import { useBorrowPreference } from '@/composables/useBorrowPreference'
import { useI18n } from 'vue-i18n'
import type { BorrowSource, BorrowRotationState } from '@/api/admin/ticketBorrowing'
import Select from '@/components/common/Select.vue'
import BorrowListPagination from './BorrowListPagination.vue'
const props = defineProps<{ sources: BorrowSource[]; rotations?: BorrowRotationState[]; groups: {id: number; name: string}[]; running: boolean; busy: boolean; rotationLoaded?: boolean; selectionDirty: boolean; scopeLoaded?: boolean; defaultModel?: string; modelOverrides?: Record<number, string>; selected: number[] }>()
const emit = defineEmits<{ open: [id: number]; records: [id: number]; rotate: [ids: number[]]; manage: []; remove: [ids: number[]]; defaults: []; batchSettings: []; 'update:selected': [ids: number[]] }>()
const { t } = useI18n()
const search = useBorrowPreference<string>('source:search', ''), filter = useBorrowPreference<string>('source:filter', 'all'), group = useBorrowPreference<number>('source:group', 0), sort = useBorrowPreference<string>('source:sort', 'id'), page = useBorrowPreference<number>('source:page', 1), pageSize = useBorrowPreference<number>('source:pageSize', 20), selectionCleared = ref(false)
const filters = ['all', 'unavailable']
const groupIndex = computed(() => new Map(props.groups.map(g => [g.id, g.name])))
function formatReset(value: string) { const date = new Date(value); return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString() }
function origin(s: BorrowSource) { return [s.explicit ? t('ticketBorrowing.explicitAccount') : '', ...(s.selected_group_ids ?? []).map(id => groupIndex.value.get(id) || `#${id}`)].filter(Boolean).join(' · ') || '—' }
const filtered = computed(() => props.sources.filter(s => {
  const query = search.value.trim().toLowerCase()
  return `${s.account_id} ${s.name || ''} ${s.pool_entry_id || ''} ${s.pool_endpoint || ''} ${s.fingerprint_id || ''}`.toLowerCase().includes(query)
    && (!group.value || s.selected_group_ids?.includes(group.value))
    && (filter.value === 'unavailable' ? !!s.unavailable_reason : true)
}).sort((a,b) => sort.value === 'name' ? (a.name || '').localeCompare(b.name || '') || a.account_id-b.account_id : a.account_id-b.account_id))
const rows = computed(() => filtered.value.slice((page.value-1)*pageSize.value, page.value*pageSize.value))
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length/pageSize.value)))
watch([pages, () => filtered.value.length], ([n, count]) => {
  // Preserve the preferred page while initial data is loading, then clamp it.
  if (count > 0) page.value = Math.max(1, Math.min(Math.floor(page.value), n))
}, { immediate: true })
watch([search, filter, group], () => { page.value = 1; selectionCleared.value = props.selected.length > 0; emit('update:selected', []) })
watch([sort, pageSize], () => { page.value = 1 })
const selectablePage = computed(() => rows.value.filter(s => s.account_id > 0).map(s => s.account_id))
const allPageSelected = computed(() => selectablePage.value.length > 0 && selectablePage.value.every(id => props.selected.includes(id)))
const somePageSelected = computed(() => selectablePage.value.some(id => props.selected.includes(id)))
function selectIds(ids: number[]) { emit('update:selected', [...new Set([...props.selected, ...ids])]); selectionCleared.value = false }
function selectPage(checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, ...selectablePage.value])] : props.selected.filter(id => !selectablePage.value.includes(id))); selectionCleared.value = false }
function selectOne(id: number, checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, id])] : props.selected.filter(v => v !== id)); selectionCleared.value = false }
</script>
<style scoped src="./borrowAccountList.css"></style>
