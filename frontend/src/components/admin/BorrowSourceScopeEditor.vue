<template>
  <fieldset :disabled="disabled" class="space-y-4" data-testid="source-scope-editor">
    <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.scope.hint') }}</p>
    <div v-if="missingCount" class="rounded-lg bg-amber-50 p-3 text-sm dark:bg-amber-900/20">
      <p>{{ t('ticketBorrowing.missingSelection', { count: missingCount }) }}</p>
      <button type="button" class="mt-2 text-primary-600" data-testid="scope-clean-missing" @click="cleanMissing">{{ t('ticketBorrowing.scope.cleanMissing') }}</button>
    </div>
    <details class="rounded-lg border p-3 dark:border-dark-600">
      <summary class="cursor-pointer font-medium">{{ t('ticketBorrowing.selectGroups') }} · {{ modelValue.group_ids.length }}</summary>
      <p class="my-2 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.scope.groupHint') }}</p>
      <input v-model="groupSearch" class="input" :aria-label="t('ticketBorrowing.searchGroups')" :placeholder="t('ticketBorrowing.searchGroups')" />
      <div class="my-2 flex flex-wrap gap-3 text-sm">
        <button type="button" class="text-primary-600" :disabled="!filteredGroups.length" data-testid="scope-groups-add" @click="changeGroups(filteredGroups.map(g => g.id), true)">{{ t('ticketBorrowing.scope.addGroups', { count: filteredGroups.length }) }}</button>
        <button type="button" class="text-primary-600" :disabled="!filteredGroups.some(g => modelValue.group_ids.includes(g.id))" data-testid="scope-groups-remove" @click="changeGroups(filteredGroups.map(g => g.id), false)">{{ t('ticketBorrowing.scope.removeGroups') }}</button>
      </div>
      <div class="max-h-48 overflow-y-auto">
        <label v-for="group in filteredGroups" :key="group.id" class="flex items-center gap-2 p-2 text-sm">
          <input type="checkbox" :data-group-id="group.id" :checked="modelValue.group_ids.includes(group.id)" @change="changeGroups([group.id], checked($event))" />
          {{ group.name }} <span class="text-gray-500 dark:text-gray-400">#{{ group.id }}</span>
        </label>
        <p v-if="!filteredGroups.length" class="p-2 text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.noMatches') }}</p>
      </div>
    </details>
    <div class="grid gap-2 sm:grid-cols-2">
      <input v-model="search" class="input" data-testid="scope-search" :aria-label="t('ticketBorrowing.searchAccounts')" :placeholder="t('ticketBorrowing.scope.search')" />
      <Select :disabled="disabled" v-model="filter" :options="filters.map(value => ({ value, label: t(`ticketBorrowing.scope.filters.${value}`) }))" :searchable="false" :aria-label="t('ticketBorrowing.scope.status')" data-testid="scope-filter" />
      <Select :disabled="disabled" v-model="group" :options="[{ value: 0, label: t('ticketBorrowing.scope.allGroups') }, ...groups.map(g => ({ value: g.id, label: g.name }))]" :aria-label="t('ticketBorrowing.scope.allGroups')" />
      <Select :disabled="disabled" v-model="health" :options="['all', 'eligible', 'unavailable'].map(value => ({ value, label: t(`ticketBorrowing.scope.health.${value}`) }))" :searchable="false" :aria-label="t('ticketBorrowing.eligibility')" />
    </div>
    <div class="flex flex-wrap items-center gap-3 rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-700">
      <label class="flex items-center gap-2"><input type="checkbox" :checked="allPage" :indeterminate="somePage && !allPage" :disabled="!rows.length" data-testid="scope-select-page" @change="toggle(pageIds, checked($event))" />{{ t('ticketBorrowing.management.selectPage') }}</label>
      <button type="button" class="text-primary-600" :disabled="!filtered.length" data-testid="scope-select-matching" @click="toggle(filteredIds, true)">{{ t('ticketBorrowing.scope.selectMatching', { count: filtered.length }) }}</button>
      <button type="button" class="text-primary-600" :disabled="!selected.length" @click="toggle(filteredIds, false)">{{ t('ticketBorrowing.scope.uncheckMatching') }}</button>
      <span>{{ t('ticketBorrowing.scope.checked', { count: selected.length }) }}</span>
      <button type="button" class="text-primary-600" :disabled="!selected.length" @click="selected = []">{{ t('ticketBorrowing.scope.clearChecks') }}</button>
    </div>
    <p v-if="selectionCleared" role="status" class="text-xs text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.management.selectionCleared') }}</p>
    <div class="flex flex-wrap gap-2">
      <button type="button" class="btn btn-primary btn-sm" :disabled="!addable.length" data-testid="scope-add" @click="changeAccounts(addable, true)">{{ t('ticketBorrowing.scope.add', { count: addable.length }) }}</button>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="!removable.length" data-testid="scope-remove" @click="changeAccounts(removable, false)">{{ t('ticketBorrowing.scope.remove', { count: removable.length }) }}</button>
      <span v-if="skippedTargets" class="self-center text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.scope.skipTargets', { count: skippedTargets }) }}</span>
    </div>
    <div class="divide-y rounded-lg border dark:divide-dark-600 dark:border-dark-600">
      <div v-for="account in rows" :key="account.id" class="flex items-start gap-3 p-3 text-sm" :data-scope-row="account.id" :data-source-account-id="includedInScope(account, modelValue) ? account.id : undefined">
        <input type="checkbox" class="mt-1" :aria-label="t('ticketBorrowing.scope.checkAccount', { id: account.id })" :data-account-id="account.id" :checked="selected.includes(account.id)" @change="toggle([account.id], checked($event))" />
        <span class="min-w-0 flex-1 break-words">{{ account.name }} <span class="text-gray-500 dark:text-gray-400">#{{ account.id }}</span>
          <span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">{{ origin(account) }}</span>
          <BorrowTag v-if="account.is_target || !account.request_eligible" class="mt-1" :value="account.is_target ? 'source_is_target' : account.unavailable_reason || 'source_account_unavailable'" detail />
        </span>
        <BorrowTag class="shrink-0" kind="state" :value="isExcluded(account.id) ? 'excluded' : includedInScope(account, modelValue) ? 'included' : 'unselected'" /><Toggle :model-value="includedInScope(account, modelValue)" :disabled="disabled || account.is_target" :aria-label="t('ticketBorrowing.scope.includeAccount', { id: account.id })" :data-scope-include="account.id" @update:model-value="changeAccounts([account.id], $event)" />
      </div>
      <p v-if="!rows.length" class="p-6 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.noMatches') }}</p>
    </div>
    <BorrowListPagination v-if="filtered.length" v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />

  </fieldset>
</template>

<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowSelection, BorrowSourceOption } from '@/api/admin/ticketBorrowing'
import { changeSourceScope, includedByGroup, includedInScope } from '@/composables/borrowSourceScope'
import { useBorrowBatchSelection } from '@/composables/useBorrowBatchSelection'
import Toggle from '@/components/common/Toggle.vue'
import Select from '@/components/common/Select.vue'
import BorrowListPagination from './BorrowListPagination.vue'

const props = defineProps<{ modelValue: BorrowSelection; accounts: BorrowSourceOption[]; groups: { id: number; name: string }[]; disabled: boolean }>()
const emit = defineEmits<{ change: [value: BorrowSelection] }>()
const { t } = useI18n()
const search = ref(''), groupSearch = ref(''), filter = ref('all'), health = ref('all'), group = ref(0)
const page = ref(1), pageSize = ref(20), selectionCleared = ref(false)
const filters = ['all', 'included', 'unselected', 'excluded']
const checked = (event: Event) => (event.target as HTMLInputElement).checked
const matches = (id: number, name: string, query: string) => {
  const q = query.trim().toLowerCase()
  return /^#?\d+$/.test(q) ? id === Number(q.replace('#', '')) : name.toLowerCase().includes(q)
}
const isExcluded = (id: number) => (props.modelValue.excluded_account_ids ?? []).includes(id)
const filteredGroups = computed(() => props.groups.filter(g => matches(g.id, g.name, groupSearch.value)))
const filtered = computed(() => props.accounts.filter(a => matches(a.id, a.name, search.value)
  && (!group.value || a.group_ids.includes(group.value))
  && (filter.value === 'included' ? includedInScope(a, props.modelValue) : filter.value === 'excluded' ? isExcluded(a.id) : filter.value === 'unselected' ? !includedInScope(a, props.modelValue) && !isExcluded(a.id) : true)
  && (health.value === 'eligible' ? a.request_eligible && !a.is_target : health.value === 'unavailable' ? !a.request_eligible || a.is_target : true)))
const rows = computed(() => filtered.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
const pageIds = computed(() => rows.value.map(a => a.id)), filteredIds = computed(() => filtered.value.map(a => a.id))
const { selected, allPage, somePage, toggle } = useBorrowBatchSelection(pageIds, filteredIds)
watch([search, filter, group, health], () => { page.value = 1; selectionCleared.value = selected.value.length > 0; selected.value = [] }, { flush: 'sync' })
watch(pageSize, () => { page.value = 1 })
watch(() => filtered.value.length, count => { page.value = Math.min(page.value, Math.max(1, Math.ceil(count / pageSize.value))) })
const selectedAccounts = computed(() => props.accounts.filter(a => selected.value.includes(a.id)))
const addable = computed(() => selectedAccounts.value.filter(a => !a.is_target && !includedInScope(a, props.modelValue)).map(a => a.id))
const removable = computed(() => selectedAccounts.value.filter(a => includedInScope(a, props.modelValue)).map(a => a.id))
const skippedTargets = computed(() => selectedAccounts.value.filter(a => a.is_target && !includedInScope(a, props.modelValue)).length)
function changeAccounts(ids: number[], add: boolean) {
  emit('change', changeSourceScope(props.modelValue, props.accounts, ids, add))
  selected.value = []
}
function changeGroups(ids: number[], add: boolean) {
  const groupIds = add ? [...new Set([...props.modelValue.group_ids, ...ids])] : props.modelValue.group_ids.filter(id => !ids.includes(id))
  emit('change', { ...props.modelValue, group_ids: groupIds })
}
function origin(account: BorrowSourceOption) {
  const names = props.groups.filter(g => account.group_ids.includes(g.id) && props.modelValue.group_ids.includes(g.id)).map(g => g.name)
  if (props.modelValue.account_ids.includes(account.id)) names.push(t('ticketBorrowing.explicitAccount'))
  if (isExcluded(account.id)) names.unshift(t('ticketBorrowing.scope.exclusionPersists'))
  return names.join(' · ') || t(includedByGroup(account, props.modelValue) ? 'ticketBorrowing.includedByGroup' : 'ticketBorrowing.scope.noRule')
}
const accountIds = computed(() => new Set(props.accounts.map(a => a.id)))
const groupIds = computed(() => new Set(props.groups.map(g => g.id)))
const missingCount = computed(() => props.modelValue.account_ids.filter(id => !accountIds.value.has(id)).length
  + (props.modelValue.excluded_account_ids ?? []).filter(id => !accountIds.value.has(id)).length
  + props.modelValue.group_ids.filter(id => !groupIds.value.has(id)).length)
function cleanMissing() {
  emit('change', { ...props.modelValue, account_ids: props.modelValue.account_ids.filter(id => accountIds.value.has(id)),
    group_ids: props.modelValue.group_ids.filter(id => groupIds.value.has(id)), excluded_account_ids: (props.modelValue.excluded_account_ids ?? []).filter(id => accountIds.value.has(id)) })
}
</script>
