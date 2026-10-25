<template>
  <BaseDialog :show="true" :title="t('ticketBorrowing.rotation.bulk.title')" :show-close-button="!busy" :close-on-escape="!busy" @close="!busy && $emit('close')">
    <form class="space-y-4" data-testid="rotation-batch-form" @submit.prevent="save">
      <p class="text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.rotation.bulk.hint') }}</p>
      <fieldset :disabled="busy || !!results" class="space-y-4">
        <fieldset class="space-y-2">
          <legend class="input-label">{{ t('ticketBorrowing.rotation.bulk.scope') }}</legend>
          <label class="flex items-center gap-2 text-sm"><input v-model="scope" type="radio" value="selected" name="rotation-batch-scope" :disabled="!selectedAccounts.length" data-testid="rotation-batch-selected" />{{ t('ticketBorrowing.rotation.bulk.selected', { count: selectedAccounts.length }) }}</label>
          <label class="flex items-center gap-2 text-sm"><input v-model="scope" type="radio" value="all" name="rotation-batch-scope" data-testid="rotation-batch-all" />{{ t('ticketBorrowing.rotation.bulk.all', { count: accounts.length }) }}</label>
          <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.rotation.bulk.scopeHint') }}</p>
        </fieldset>
        <div>
          <label for="rotation-batch-automatic" class="input-label">{{ t('ticketBorrowing.rotation.automatic') }}</label>
          <select id="rotation-batch-automatic" v-model="automatic" class="input" data-testid="rotation-batch-automatic">
            <option value="keep">{{ t('ticketBorrowing.rotation.bulk.keep') }}</option>
            <option value="enable">{{ t('ticketBorrowing.rotation.bulk.enable') }}</option>
            <option value="disable">{{ t('ticketBorrowing.rotation.bulk.disable') }}</option>
          </select>
        </div>
        <div>
          <label class="flex items-center gap-2 text-sm"><input v-model="changeInterval" type="checkbox" data-testid="rotation-batch-change-interval" />{{ t('ticketBorrowing.rotation.bulk.changeInterval') }}</label>
          <div v-if="changeInterval" class="mt-2">
            <label for="rotation-batch-interval" class="input-label">{{ t('ticketBorrowing.rotation.interval') }}（{{ t('ticketBorrowing.secondsUnit') }}）</label>
            <input id="rotation-batch-interval" v-model.number="interval" type="number" min="1" max="86400" step="1" class="input" :aria-invalid="!validInterval" aria-describedby="rotation-batch-interval-hint" data-testid="rotation-batch-interval" />
            <p id="rotation-batch-interval-hint" class="mt-1.5 text-xs" :class="validInterval ? 'text-gray-500' : 'text-red-600'">{{ t(validInterval ? 'ticketBorrowing.rotation.intervalHint' : 'ticketBorrowing.rotation.intervalError') }}</p>
          </div>
          <p v-else class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.rotation.bulk.keepInterval') }}</p>
        </div>
      </fieldset>
      <p v-if="!running" class="text-xs leading-5 text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.rotation.pausedHint') }}</p>
      <p class="text-sm font-medium" data-testid="rotation-batch-count">{{ t('ticketBorrowing.rotation.bulk.affected', { count: targets.length }) }}</p>
      <p v-if="targets.length > 1000" role="alert" class="text-sm text-red-600">{{ t('ticketBorrowing.rotation.bulk.limit') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <div v-if="results" class="space-y-2" data-testid="rotation-batch-result">
        <div class="flex flex-wrap items-center gap-2" role="status"><span class="inline-flex items-center gap-1" data-batch-count="saved"><BorrowTag kind="state" value="saved" /> {{ results.length - failed.length }}</span><span class="inline-flex items-center gap-1" data-batch-count="failed"><BorrowTag kind="state" value="failed" /> {{ failed.length }}</span></div>
        <template v-if="failed.length">
          <p class="text-xs leading-5 text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.rotation.bulk.retryHint') }}</p>
          <ul class="max-h-48 space-y-2 overflow-y-auto rounded-lg bg-red-50 p-3 text-xs text-red-700 dark:bg-red-900/20 dark:text-red-300">
            <li v-for="item in failed" :key="item.account_id">{{ accountNames.get(item.account_id) || `#${item.account_id}` }} · #{{ item.account_id }}：<BorrowTag v-if="item.reason" :value="item.reason" detail /><BorrowTag v-else kind="state" value="failed" /></li>
          </ul>
        </template>
      </div>
      <div class="flex flex-wrap justify-end gap-2 border-t border-gray-200 pt-4 dark:border-dark-600">
        <button type="button" class="btn btn-secondary" :disabled="busy" @click="$emit('close')">{{ t(results ? 'ticketBorrowing.rotation.bulk.close' : 'ticketBorrowing.rotation.bulk.cancel') }}</button>
        <button v-if="failed.length" type="button" class="btn btn-primary" :disabled="busy" data-testid="batch-retry" @click="$emit('retry')">{{ t('ticketBorrowing.management.prepareRetry') }}</button>
        <button v-if="!results" type="submit" class="btn btn-primary" :disabled="!canSave" data-testid="rotation-batch-save">{{ t(busy ? 'ticketBorrowing.rotation.bulk.saving' : 'ticketBorrowing.rotation.bulk.save') }}</button>
      </div>
    </form>
  </BaseDialog>
</template>

<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { BorrowRotationBatchAccount, BorrowRotationBatchInput, BorrowRotationBatchResult } from '@/api/admin/ticketBorrowing'

const props = defineProps<{ accounts: BorrowRotationBatchAccount[]; selectedIds: number[]; busy: boolean; running: boolean; error: string; results: BorrowRotationBatchResult[] | null }>()
const emit = defineEmits<{ close: []; retry: []; save: [input: BorrowRotationBatchInput]; dirty: [value: boolean] }>()
const { t } = useI18n()
const selectedAccounts = computed(() => props.accounts.filter(a => props.selectedIds.includes(a.account_id)))
const scope = ref(selectedAccounts.value.length ? 'selected' : 'all')
const automatic = ref('keep'), changeInterval = ref(false), interval = ref(60)
const targets = computed(() => scope.value === 'all' ? props.accounts : selectedAccounts.value)
const accountNames = computed(() => new Map(props.accounts.map(a => [a.account_id, a.name])))
const failed = computed(() => props.results?.filter(item => item.status === 'failed') ?? [])
const dirty = computed(() => !props.results && (automatic.value !== 'keep' || changeInterval.value))
const validInterval = computed(() => !changeInterval.value || Number.isInteger(interval.value) && interval.value >= 1 && interval.value <= 86400)
const canSave = computed(() => !props.busy && dirty.value && validInterval.value && targets.value.length > 0 && targets.value.length <= 1000)
watch(dirty, value => emit('dirty', value), { immediate: true })
function save() {
  if (!canSave.value) return
  emit('save', {
    accounts: targets.value.map(({ account_id, revision }) => ({ account_id, revision })),
    ...(automatic.value === 'keep' ? {} : { automatic: automatic.value === 'enable' }),
    ...(changeInterval.value ? { interval_seconds: interval.value } : {})
  })
}
</script>
