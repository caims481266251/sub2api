<template>
  <section class="min-w-0 space-y-4 rounded-xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-600 dark:bg-dark-900/30 sm:p-5" :data-rotation-source="accountId">
    <div>
      <h3 class="break-words text-sm font-semibold">{{ name }} <span class="font-normal text-gray-500 dark:text-gray-400">#{{ accountId }}</span></h3>
      <p class="mt-1.5 max-w-3xl text-xs leading-5 text-gray-600 dark:text-gray-400">{{ t('ticketBorrowing.rotation.policyHint') }}</p>
    </div>
    <dl v-if="state?.pool_entry_id" class="grid gap-3 rounded-lg bg-white p-3 text-sm dark:bg-dark-800 sm:grid-cols-2">
      <div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.poolBinding') }}</dt><dd class="mt-1 break-all">#{{ state.pool_entry_id }} · {{ state.pool_endpoint }}</dd></div>
      <div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.fixedFingerprint') }}</dt><dd class="mt-1 break-all font-mono text-xs">{{ state.fingerprint_id }}</dd></div>
    </dl>
    <p v-if="state?.selection_kind" class="text-xs leading-5 text-gray-600 dark:text-gray-400" data-testid="rotation-choice">{{ t(`ticketBorrowing.rotation.choices.${state.selection_kind}`) }}</p>
    <div v-if="state?.reason" class="space-y-1"><p class="text-xs text-gray-500 dark:text-gray-400">{{ t(state.active_job ? 'ticketBorrowing.tags.currentExecution' : 'ticketBorrowing.tags.lastRotation') }}</p><BorrowTag :value="state.reason" detail /></div>
    <div class="flex flex-wrap gap-2"><BorrowTag kind="state" :value="!loaded || !state ? 'unknown' : state.automatic ? 'autoEnabled' : 'autoDisabled'" /><BorrowTag v-if="loaded && state?.active_job" kind="state" value="rotationActive" /><BorrowTag v-else-if="loaded && state?.automatic" kind="state" :value="running ? 'rotationWaiting' : 'paused'" /></div>
    <dl class="grid gap-3 text-sm sm:grid-cols-2">
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.rotation.last') }}</dt><dd class="mt-1 tabular-nums">{{ date(state?.last_at) }}</dd></div>
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.rotation.next') }}</dt><dd class="mt-1 tabular-nums">{{ loaded && state?.automatic && running && !state.active_job ? date(state.next_at) : '—' }}</dd></div>
    </dl>
    <div class="grid items-start gap-4 border-t border-gray-200 pt-4 dark:border-dark-600 sm:grid-cols-2">
      <div class="min-w-0">
        <span class="input-label">{{ t('ticketBorrowing.rotation.automatic') }}</span>
        <label class="flex min-h-11 cursor-pointer items-center gap-3 rounded-xl border border-gray-200 bg-white px-3 py-2 text-sm dark:border-dark-600 dark:bg-dark-800">
          <input v-model="draft.automatic" type="checkbox" class="h-4 w-4 shrink-0 accent-primary-600" :disabled="busy || !loaded" :aria-label="t('ticketBorrowing.rotation.automatic')" data-testid="rotation-auto" />
          <BorrowTag kind="state" :value="draft.automatic ? 'enabled' : 'disabled'" /><BorrowTag v-if="dirty" kind="state" value="unsaved" />
        </label>
      </div>
      <div class="min-w-0">
        <label :for="`rotation-interval-${accountId}`" class="input-label">{{ t('ticketBorrowing.rotation.interval') }}</label>
        <div class="relative max-w-xs">
          <input :id="`rotation-interval-${accountId}`" v-model.number="draft.interval_seconds" type="number" min="1" max="86400" step="1" class="input h-11 pr-12 tabular-nums" :class="{ 'input-error': !valid }" :disabled="busy || !loaded" :aria-invalid="!valid" :aria-describedby="`rotation-interval-hint-${accountId}`" data-testid="rotation-interval" />
          <span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400" aria-hidden="true">{{ t('ticketBorrowing.secondsUnit') }}</span>
        </div>
        <p :id="`rotation-interval-hint-${accountId}`" class="mt-1.5 text-xs leading-5" :class="valid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(valid ? 'ticketBorrowing.rotation.intervalHint' : 'ticketBorrowing.rotation.intervalError') }}</p>
      </div>
    </div>
    <div class="flex flex-wrap items-center gap-2 border-t border-gray-200 pt-4 dark:border-dark-600">
      <button type="button" class="btn w-full sm:w-auto" :class="dirty && valid && !conflict ? 'btn-primary' : 'btn-secondary'" :disabled="busy || !loaded || !dirty || !valid || conflict" data-testid="rotation-save" @click="$emit('save', { ...draft })">{{ t('ticketBorrowing.rotation.save') }}</button>
      <button type="button" class="btn btn-secondary w-full sm:w-auto" :disabled="busy || !loaded || canRotate === false || !hasBinding || !!state?.active_job" data-testid="rotation-now" @click="$emit('rotate')">{{ t('ticketBorrowing.rotation.now') }}</button>
      <button v-if="dirty || conflict" type="button" class="btn btn-ghost w-full sm:w-auto" :disabled="busy" data-testid="rotation-revert" @click="reset">{{ t('ticketBorrowing.revert') }}</button>
    </div>
    <BorrowTag v-if="rotationUnavailableReason" :value="rotationUnavailableReason" detail />
    <p v-if="conflict" role="alert" class="text-sm text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.remoteChange') }}</p>
    <p v-if="!running" class="text-xs leading-5 text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.rotation.pausedHint') }}</p>
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, watch } from 'vue'
import { useBorrowEditorState, type BorrowEditorState } from '@/composables/useBorrowEditorState'
import { useI18n } from 'vue-i18n'
import type { BorrowRotationState, BorrowRotationPolicy } from '@/api/admin/ticketBorrowing'
const props = defineProps<{ accountId: number; name: string; hasBinding: boolean; canRotate?: boolean; rotationUnavailableReason?: string; state?: BorrowRotationState; busy: boolean; running: boolean; loaded: boolean; editorState?: BorrowEditorState<BorrowRotationPolicy> }>()
const emit = defineEmits<{ save: [policy: BorrowRotationPolicy]; rotate: []; dirty: [value: boolean]; editorState: [value: BorrowEditorState<BorrowRotationPolicy>] }>()
const { t } = useI18n()
const value = (): BorrowRotationPolicy => ({revision: props.state?.revision ?? 0, automatic: props.state?.automatic ?? false, interval_seconds: props.state?.interval_seconds ?? 60})
const { draft, baseline, dirty } = useBorrowEditorState(value(), props.editorState, state => emit('editorState', state))
const valid = computed(() => Number.isInteger(draft.value.interval_seconds) && draft.value.interval_seconds >= 1 && draft.value.interval_seconds <= 86400)
const conflict = computed(() => draft.value.revision !== (props.state?.revision ?? 0))
function reset() { draft.value = value(); baseline.value = JSON.stringify(draft.value) }
watch(() => props.state, () => {
 const saved = value()
 // A successful save returns the same values with a new revision.
 if (!dirty.value || (saved.automatic === draft.value.automatic && saved.interval_seconds === draft.value.interval_seconds)) reset()
}, { immediate: true })
watch(dirty, v => emit('dirty', v), { immediate: true })
function date(raw?: string) { return raw ? new Date(raw).toLocaleString() : '—' }
</script>
