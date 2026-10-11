<template>
  <section class="borrow-list card" data-testid="source-list">
    <div class="borrow-toolbar">
      <div><h2 class="font-semibold">{{ t('ticketBorrowing.sources') }} <span class="ml-1 text-sm font-normal text-gray-500">{{ sources.length }}</span></h2></div>
      <div class="flex flex-wrap gap-2">
        <button class="btn btn-secondary btn-sm" :disabled="busy || selectionDirty || !rotationLoaded || !allBound.length || allBound.some(id => rotationIndex.get(id)?.active_job)" :title="t('ticketBorrowing.rotation.allHint')" data-testid="rotate-all" @click="$emit('rotate', allBound)">{{ t('ticketBorrowing.rotation.all', { count: allBound.length }) }}</button>
        <button class="btn btn-secondary btn-sm" :disabled="busy || selectionDirty || !rotationLoaded || !configurableSources.length" data-testid="source-batch-settings" @click="$emit('batchSettings')">{{ t('ticketBorrowing.rotation.bulk.title') }}</button><button class="btn btn-secondary btn-sm" data-testid="source-defaults" @click="$emit('defaults')">{{ t('ticketBorrowing.management.defaults') }}</button><button class="btn btn-primary btn-sm" data-testid="manage-sources" @click="$emit('manage')">{{ t('ticketBorrowing.management.manageSources') }}</button></div>
    </div>
    <p v-if="sources.length > allBound.length" class="px-4 py-2 text-xs text-amber-700 dark:text-amber-300" data-testid="rotation-excluded">{{ t('ticketBorrowing.rotation.excluded', { count: sources.length - allBound.length }) }}</p>
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
      <button class="borrow-link" :disabled="busy || selectionDirty || !rotationLoaded || !selected.some(id => allBound.includes(id))" data-testid="rotate-selected" @click="$emit('rotate', [...selected])">{{ t('ticketBorrowing.rotation.batch', { count: selected.length }) }}</button>
      <button v-if="selected.length" class="borrow-link" @click="$emit('update:selected', [])">{{ t('ticketBorrowing.scope.clearChecks') }}</button>
    </div>
    <p v-if="selectionCleared" role="status" class="px-4 py-2 text-xs text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.management.selectionCleared') }}</p>
    <div v-if="rows.length" class="borrow-table-wrap">
      <table class="borrow-table">
        <thead><tr><th class="w-[20%]">{{ t('ticketBorrowing.name') }}</th><th class="w-[17%]">{{ t('ticketBorrowing.eligibility') }}</th><th class="w-[15%]">{{ t('ticketBorrowing.model') }}</th><th class="w-[22%]">{{ t('ticketBorrowing.poolBinding') }}</th><th class="w-[14%]">{{ t('ticketBorrowing.rotation.automatic') }}</th><th class="w-[12%]">{{ t('ticketBorrowing.management.actions') }}</th></tr></thead>
        <tbody><tr v-for="source in rows" :key="source.account_id" :data-saved-source-id="source.account_id">
          <td class="borrow-account-cell"><div class="flex min-w-0 items-center gap-2"><input v-if="source.account_id > 0" :checked="selected.includes(source.account_id)" type="checkbox" :disabled="busy" :aria-label="t('ticketBorrowing.scope.checkAccount', { id: source.account_id })" :data-rotation-select="source.account_id" @change="selectOne(source.account_id, ($event.target as HTMLInputElement).checked)" /><button class="borrow-account" :title="source.name" @click="$emit('open', source.account_id)">{{ source.name || `#${source.account_id}` }}</button></div><span class="borrow-subline">#{{ source.account_id }} · {{ origin(source) }}</span></td>
          <td :data-label="t('ticketBorrowing.eligibility')"><span class="borrow-badge" :class="source.unavailable_reason ? 'borrow-badge-warn' : 'borrow-badge-good'">{{ t(source.unavailable_reason === 'source_rate_limited' ? 'ticketBorrowing.management.rateLimited' : source.unavailable_reason ? 'ticketBorrowing.management.needsAttention' : 'ticketBorrowing.management.eligible') }}</span><span class="borrow-subline" :title="result(source)">{{ result(source) }}</span><span v-if="source.unavailable_reason === 'source_rate_limited' && source.rate_limit_reset_at" class="borrow-subline" data-testid="source-rate-limit-reset">{{ t('ticketBorrowing.management.resetsAt', { time: formatReset(source.rate_limit_reset_at) }) }}</span></td>
          <td :data-label="t('ticketBorrowing.model')"><span class="borrow-truncate" :title="source.model">{{ source.model || '—' }}</span></td>
          <td :data-label="t('ticketBorrowing.poolBinding')" :data-pool-binding="source.account_id"><template v-if="source.use_proxy_pool"><span v-if="source.pool_entry_id" class="borrow-truncate" :title="source.pool_endpoint">#{{ source.pool_entry_id }} · {{ source.pool_endpoint }}</span><span v-else class="text-amber-600">{{ t('ticketBorrowing.poolBindingWaiting') }}</span><span class="borrow-subline font-mono" :title="source.fingerprint_id">{{ source.fingerprint_id || '—' }}</span></template><span v-else class="text-gray-500">{{ t('ticketBorrowing.accountProxy') }}</span></td>
          <td :data-label="t('ticketBorrowing.rotation.automatic')"><span>{{ t(rotationIndex.get(source.account_id)?.automatic ? 'ticketBorrowing.enabled' : 'ticketBorrowing.disabled') }}<template v-if="source.use_proxy_pool"> · {{ rotationIndex.get(source.account_id)?.interval_seconds ?? 60 }}s</template></span><span class="borrow-subline">{{ rotationIndex.get(source.account_id)?.active_job ? t('ticketBorrowing.rotation.inProgress') : rotationIndex.get(source.account_id)?.automatic && !running ? t('ticketBorrowing.paused') : '—' }}</span></td>
          <td class="borrow-actions"><button class="borrow-link" :data-source-open="source.account_id" @click="$emit('open', source.account_id)">{{ t('ticketBorrowing.management.detailsSettings') }}</button><button v-if="source.use_proxy_pool" class="borrow-link" :title="source.rotation_unavailable_reason ? t(`ticketBorrowing.reasons.${source.rotation_unavailable_reason}`) : undefined" :data-rotate-source="source.account_id" :disabled="busy || selectionDirty || !rotationLoaded || source.can_rotate !== true || !source.pool_entry_id || source.unavailable_reason === 'source_is_target' || !!rotationIndex.get(source.account_id)?.active_job" @click="$emit('rotate', [source.account_id])">{{ t('ticketBorrowing.rotation.now') }}</button></td>
        </tr></tbody>
      </table>
    </div>
    <div v-else class="borrow-empty"><p>{{ t(sources.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.selectSourceHint') }}</p><button v-if="sources.length" class="borrow-link mt-2" @click="search = ''; filter = 'all'; group = 0">{{ t('ticketBorrowing.management.resetFilters') }}</button></div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
  </section>
</template>
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowSource, BorrowRotationState } from '@/api/admin/ticketBorrowing'
import Select from '@/components/common/Select.vue'
import BorrowListPagination from './BorrowListPagination.vue'
const props = defineProps<{ sources: BorrowSource[]; rotations: BorrowRotationState[]; groups: {id: number; name: string}[]; running: boolean; busy: boolean; rotationLoaded: boolean; selectionDirty: boolean; scopeLoaded?: boolean; selected: number[] }>()
const emit = defineEmits<{ open: [id: number]; rotate: [ids: number[]]; manage: []; remove: [ids: number[]]; defaults: []; batchSettings: []; 'update:selected': [ids: number[]] }>()
const { t, te } = useI18n()
const search = ref(''), filter = ref('all'), group = ref(0), sort = ref('id'), page = ref(1), pageSize = ref(20), selectionCleared = ref(false)
const filters = ['all', 'unavailable', 'unbound', 'automatic', 'manual']
const rotationIndex = computed(() => new Map(props.rotations.map(r => [r.account_id, r])))
const groupIndex = computed(() => new Map(props.groups.map(g => [g.id, g.name])))
function formatReset(value: string) { const date = new Date(value); return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString() }
function origin(s: BorrowSource) { return [s.explicit ? t('ticketBorrowing.explicitAccount') : '', ...(s.selected_group_ids ?? []).map(id => groupIndex.value.get(id) || `#${id}`)].filter(Boolean).join(' · ') || '—' }
function result(s: BorrowSource) { const code = s.unavailable_reason || s.last_reason; const key = `ticketBorrowing.reasons.${code}`; return code ? te(key) ? t(key) : t('ticketBorrowing.unknownResult') : '—' }
const filtered = computed(() => props.sources.filter(s => {
  const query = search.value.trim().toLowerCase()
  return `${s.account_id} ${s.name || ''} ${s.pool_entry_id || ''} ${s.pool_endpoint || ''} ${s.fingerprint_id || ''}`.toLowerCase().includes(query)
    && (!group.value || s.selected_group_ids?.includes(group.value))
    && (filter.value === 'unavailable' ? !!s.unavailable_reason : filter.value === 'unbound' ? s.use_proxy_pool && !s.pool_entry_id : filter.value === 'automatic' ? rotationIndex.value.get(s.account_id)?.automatic : filter.value === 'manual' ? !rotationIndex.value.get(s.account_id)?.automatic : true)
}).sort((a,b) => sort.value === 'name' ? (a.name || '').localeCompare(b.name || '') || a.account_id-b.account_id : a.account_id-b.account_id))
const rows = computed(() => filtered.value.slice((page.value-1)*pageSize.value, page.value*pageSize.value))
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length/pageSize.value)))
watch(pages, n => { page.value = Math.min(page.value, n) })
watch([search, filter, group], () => { page.value = 1; selectionCleared.value = props.selected.length > 0; emit('update:selected', []) })
watch([sort, pageSize], () => { page.value = 1 })
const allBound = computed(() => props.sources.filter(s => s.can_rotate === true && s.use_proxy_pool && s.pool_entry_id && s.unavailable_reason !== 'source_is_target' && !rotationIndex.value.get(s.account_id)?.active_job).map(s => s.account_id))
const configurableSources = computed(() => props.sources.filter(s => s.account_id > 0 && s.use_proxy_pool && s.unavailable_reason !== 'source_is_target'))
const selectablePage = computed(() => rows.value.filter(s => s.account_id > 0).map(s => s.account_id))
const allPageSelected = computed(() => selectablePage.value.length > 0 && selectablePage.value.every(id => props.selected.includes(id)))
const somePageSelected = computed(() => selectablePage.value.some(id => props.selected.includes(id)))
function selectIds(ids: number[]) { emit('update:selected', [...new Set([...props.selected, ...ids])]); selectionCleared.value = false }
function selectPage(checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, ...selectablePage.value])] : props.selected.filter(id => !selectablePage.value.includes(id))); selectionCleared.value = false }
function selectOne(id: number, checked: boolean) { emit('update:selected', checked ? [...new Set([...props.selected, id])] : props.selected.filter(v => v !== id)); selectionCleared.value = false }
</script>
<style scoped src="./borrowAccountList.css"></style>
