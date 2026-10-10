<template>
  <BaseDialog :show="true" :title="t('ticketBorrowing.probePolicy.bulk.title')" :show-close-button="!busy" :close-on-escape="!busy" @close="!busy && $emit('close')">
    <form class="space-y-4" data-testid="probe-batch-form" @submit.prevent="save">
      <p class="text-sm leading-6 text-gray-500">{{ t('ticketBorrowing.probePolicy.bulk.hint') }}</p>
      <fieldset :disabled="busy || !!results" class="space-y-4">
        <fieldset class="space-y-2">
          <legend class="input-label">{{ t('ticketBorrowing.probePolicy.bulk.scope') }}</legend>
          <label class="flex items-center gap-2 text-sm"><input v-model="scope" type="radio" value="selected" name="probe-batch-scope" :disabled="!selectedAccounts.length" data-testid="probe-batch-selected" />{{ t('ticketBorrowing.probePolicy.bulk.selected', { count: selectedAccounts.length }) }}</label>
          <label class="flex items-center gap-2 text-sm"><input v-model="scope" type="radio" value="all" name="probe-batch-scope" data-testid="probe-batch-all" />{{ t('ticketBorrowing.probePolicy.bulk.all', { count: accounts.length }) }}</label>
          <p class="text-xs leading-5 text-gray-500">{{ t('ticketBorrowing.probePolicy.bulk.scopeHint') }}</p>
        </fieldset>
        <div>
          <label class="flex items-center gap-2 text-sm"><input v-model="changeConcurrency" type="checkbox" data-testid="probe-batch-change-concurrency" />{{ t('ticketBorrowing.probePolicy.bulk.changeConcurrency') }}</label>
          <div v-if="changeConcurrency" class="mt-2">
            <label for="probe-batch-concurrency" class="input-label">{{ t('ticketBorrowing.probePolicy.concurrency') }}</label>
            <input id="probe-batch-concurrency" v-model.number="concurrency" type="number" min="1" max="32" step="1" class="input" :aria-invalid="!validConcurrency" aria-describedby="probe-batch-concurrency-hint" data-testid="probe-batch-concurrency" />
            <p id="probe-batch-concurrency-hint" class="mt-1.5 text-xs" :class="validConcurrency ? 'text-gray-500' : 'text-red-600'">{{ t(validConcurrency ? 'ticketBorrowing.probePolicy.concurrencyHint' : 'ticketBorrowing.probePolicy.concurrencyError') }}</p>
          </div>
          <p v-else class="mt-1.5 text-xs text-gray-500">{{ t('ticketBorrowing.probePolicy.bulk.keepConcurrency') }}</p>
        </div>
        <div>
          <label class="flex items-center gap-2 text-sm"><input v-model="changeRenew" type="checkbox" data-testid="probe-batch-change-renew" />{{ t('ticketBorrowing.probePolicy.bulk.changeRenew') }}</label>
          <div v-if="changeRenew" class="mt-2">
            <label for="probe-batch-renew" class="input-label">{{ t('ticketBorrowing.probePolicy.renewBefore') }}（{{ t('ticketBorrowing.secondsUnit') }}）</label>
            <input id="probe-batch-renew" v-model.number="renew" type="number" min="0" max="240" step="1" class="input" :aria-invalid="!validRenew" aria-describedby="probe-batch-renew-hint" data-testid="probe-batch-renew" />
            <p id="probe-batch-renew-hint" class="mt-1.5 text-xs" :class="validRenew ? 'text-gray-500' : 'text-red-600'">{{ t(validRenew ? 'ticketBorrowing.probePolicy.renewHint' : 'ticketBorrowing.probePolicy.renewError') }}</p>
          </div>
          <p v-else class="mt-1.5 text-xs text-gray-500">{{ t('ticketBorrowing.probePolicy.bulk.keepRenew') }}</p>
        </div>
      </fieldset>
      <p v-if="!running" class="text-xs leading-5 text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.probePolicy.bulk.pausedHint') }}</p>
      <p class="text-sm font-medium" data-testid="probe-batch-count">{{ t('ticketBorrowing.probePolicy.bulk.affected', { count: targets.length }) }}</p>
      <p v-if="targets.length > 1000" role="alert" class="text-sm text-red-600">{{ t('ticketBorrowing.probePolicy.bulk.limit') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <div v-if="results" class="space-y-2" data-testid="probe-batch-result">
        <p role="status" class="text-sm">{{ t('ticketBorrowing.probePolicy.bulk.result', { saved: results.length - failed.length, failed: failed.length }) }}</p>
        <template v-if="failed.length">
          <p class="text-xs leading-5 text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.probePolicy.bulk.retryHint') }}</p>
          <ul class="max-h-48 space-y-2 overflow-y-auto rounded-lg bg-red-50 p-3 text-xs text-red-700 dark:bg-red-900/20 dark:text-red-300">
            <li v-for="item in failed" :key="item.account_id">{{ accountNames.get(item.account_id) || `#${item.account_id}` }} · #{{ item.account_id }}：{{ reason(item.reason) }}</li>
          </ul>
        </template>
      </div>
      <div class="flex flex-wrap justify-end gap-2 border-t border-gray-200 pt-4 dark:border-dark-600">
        <button type="button" class="btn btn-secondary" :disabled="busy" @click="$emit('close')">{{ t(results ? 'ticketBorrowing.probePolicy.bulk.close' : 'ticketBorrowing.probePolicy.bulk.cancel') }}</button>
        <button v-if="!results" type="submit" class="btn btn-primary" :disabled="!canSave" data-testid="probe-batch-save">{{ t(busy ? 'ticketBorrowing.probePolicy.bulk.saving' : 'ticketBorrowing.probePolicy.bulk.save') }}</button>
      </div>
    </form>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { BorrowProbeBatchAccount, BorrowProbeBatchInput, BorrowProbeBatchResult } from '@/api/admin/ticketBorrowing'

const props = defineProps<{ accounts: BorrowProbeBatchAccount[]; selectedIds: number[]; busy: boolean; running: boolean; error: string; results: BorrowProbeBatchResult[] | null }>()
const emit = defineEmits<{ close: []; save: [input: BorrowProbeBatchInput]; dirty: [value: boolean] }>()
const { t, te } = useI18n()
const selectedAccounts = computed(() => props.accounts.filter(a => props.selectedIds.includes(a.account_id)))
const scope = ref(selectedAccounts.value.length ? 'selected' : 'all')
const changeConcurrency = ref(false), concurrency = ref(2), changeRenew = ref(false), renew = ref(15)
const targets = computed(() => scope.value === 'all' ? props.accounts : selectedAccounts.value)
const accountNames = computed(() => new Map(props.accounts.map(a => [a.account_id, a.name])))
const failed = computed(() => props.results?.filter(item => item.status === 'failed') ?? [])
const dirty = computed(() => !props.results && (changeConcurrency.value || changeRenew.value))
const validConcurrency = computed(() => !changeConcurrency.value || Number.isInteger(concurrency.value) && concurrency.value >= 1 && concurrency.value <= 32)
const validRenew = computed(() => !changeRenew.value || Number.isInteger(renew.value) && renew.value >= 0 && renew.value <= 240)
const canSave = computed(() => !props.busy && dirty.value && validConcurrency.value && validRenew.value && targets.value.length > 0 && targets.value.length <= 1000)
watch(dirty, value => emit('dirty', value), { immediate: true })
function reason(code?: string) { const key = `ticketBorrowing.reasons.${code}`; return te(key) ? t(key) : t('ticketBorrowing.failure') }
function save() {
  if (!canSave.value) return
  emit('save', {
    accounts: targets.value.map(({ account_id, revision }) => ({ account_id, revision })),
    ...(changeConcurrency.value ? { concurrency: concurrency.value } : {}),
    ...(changeRenew.value ? { renew_before_seconds: renew.value } : {})
  })
}
</script>
