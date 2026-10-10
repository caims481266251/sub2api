<template>
  <section class="min-w-0 rounded-xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-600 dark:bg-dark-900/30 sm:p-5" :data-probe-policy="target.account_id">
    <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('ticketBorrowing.probePolicy.title') }}</h3>
    <p class="mt-1.5 max-w-3xl text-xs leading-5 text-gray-600 dark:text-gray-400">{{ t('ticketBorrowing.probePolicy.hint') }}</p>
    <div class="mt-4 grid max-w-2xl gap-4 sm:grid-cols-2">
      <div class="min-w-0">
        <label :for="`probe-concurrency-${target.account_id}`" class="input-label">{{ t('ticketBorrowing.probePolicy.concurrency') }}</label>
        <input :id="`probe-concurrency-${target.account_id}`" v-model.number="draft.concurrency" type="number" min="1" max="32" step="1" class="input h-11 tabular-nums" :class="{ 'input-error': !concurrencyValid }" :disabled="saving || !loaded" :aria-invalid="!concurrencyValid" :aria-describedby="`probe-concurrency-hint-${target.account_id}`" data-testid="probe-concurrency" />
        <p :id="`probe-concurrency-hint-${target.account_id}`" class="mt-1.5 text-xs leading-5" :class="concurrencyValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(concurrencyValid ? 'ticketBorrowing.probePolicy.concurrencyHint' : 'ticketBorrowing.probePolicy.concurrencyError') }}</p>
      </div>
      <div class="min-w-0">
        <label :for="`probe-renew-${target.account_id}`" class="input-label">{{ t('ticketBorrowing.probePolicy.renewBefore') }}</label>
        <div class="relative">
          <input :id="`probe-renew-${target.account_id}`" v-model.number="draft.renew_before_seconds" type="number" min="0" max="240" step="1" class="input h-11 pr-12 tabular-nums" :class="{ 'input-error': !renewalValid }" :disabled="saving || !loaded" :aria-invalid="!renewalValid" :aria-describedby="`probe-renew-hint-${target.account_id}`" data-testid="probe-renew-before" />
          <span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400" aria-hidden="true">{{ t('ticketBorrowing.secondsUnit') }}</span>
        </div>
        <p :id="`probe-renew-hint-${target.account_id}`" class="mt-1.5 text-xs leading-5" :class="renewalValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(renewalValid ? 'ticketBorrowing.probePolicy.renewHint' : 'ticketBorrowing.probePolicy.renewError') }}</p>
      </div>
    </div>
    <p v-if="conflict" role="alert" class="mt-3 text-sm text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.remoteChange') }}</p>
    <p v-if="error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <div class="mt-4 flex flex-wrap items-center gap-3 border-t border-gray-200 pt-4 dark:border-dark-600">
      <button type="button" class="btn w-full sm:w-auto" :class="dirty && valid && !conflict ? 'btn-primary' : 'btn-secondary'" :disabled="saving || !loaded || !dirty || !valid || conflict" data-testid="probe-policy-save" @click="save">{{ t(saving ? 'ticketBorrowing.loading' : 'ticketBorrowing.probePolicy.save') }}</button>
      <button v-if="dirty || conflict" type="button" class="btn btn-ghost w-full sm:w-auto" :disabled="saving" data-testid="probe-policy-revert" @click="reset">{{ t('ticketBorrowing.revert') }}</button>
      <span v-if="saved && !dirty" role="status" class="text-xs text-emerald-700 dark:text-emerald-400">{{ t('ticketBorrowing.settingsSaved') }}</span>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useBorrowEditorState, type BorrowEditorState } from '@/composables/useBorrowEditorState'
import { useI18n } from 'vue-i18n'
import { ticketBorrowing, type BorrowTarget, type BorrowProbePolicy } from '@/api/admin/ticketBorrowing'
const props = defineProps<{ target: BorrowTarget; editorState?: BorrowEditorState<BorrowProbePolicy> }>()
const emit = defineEmits<{ saved: [target: BorrowTarget]; dirty: [value: boolean]; editorState: [value: BorrowEditorState<BorrowProbePolicy>]; saving: [value: boolean] }>()
const { t, te } = useI18n()
const value = (): BorrowProbePolicy => ({ revision: props.target.probe_policy?.revision ?? 0, concurrency: props.target.probe_policy?.concurrency ?? 2, renew_before_seconds: props.target.probe_policy?.renew_before_seconds ?? 15 })
const { draft, baseline, dirty } = useBorrowEditorState(value(), props.editorState, state => emit('editorState', state))
const saving = ref(false), error = ref(''), saved = ref(false)
let generation = 0
const loaded = computed(() => (props.target.probe_policy?.revision ?? 0) > 0)
const conflict = computed(() => draft.value.revision !== (props.target.probe_policy?.revision ?? 0))
const concurrencyValid = computed(() => Number.isInteger(draft.value.concurrency) && draft.value.concurrency >= 1 && draft.value.concurrency <= 32)
const renewalValid = computed(() => Number.isInteger(draft.value.renew_before_seconds) && draft.value.renew_before_seconds >= 0 && draft.value.renew_before_seconds <= 240)
const valid = computed(() => concurrencyValid.value && renewalValid.value)
function reset() { draft.value = value(); baseline.value = JSON.stringify(draft.value); error.value = ''; saved.value = false }
watch(() => props.target.probe_policy, () => { if (!dirty.value && JSON.stringify(value()) !== baseline.value) reset() }, { immediate: true })
watch(() => props.target.account_id, () => { generation++; saving.value = false; reset() })
watch(dirty, value => emit('dirty', value), { immediate: true })
watch(saving, value => emit('saving', value), { flush: 'sync' })
onBeforeUnmount(() => { generation++ })
async function save() {
  if (saving.value || !loaded.value || !valid.value || conflict.value) return
  const version = ++generation
  saving.value = true; error.value = ''; saved.value = false
  try {
    const target = await ticketBorrowing.saveProbePolicy(props.target.account_id, { ...draft.value })
    if (version !== generation) return
    draft.value = { ...target.probe_policy! }; baseline.value = JSON.stringify(draft.value)
    emit('saved', target); saved.value = true
  } catch (e) {
    if (version !== generation) return
    const detail = e as { reason?: string; message?: string }
    const key = `ticketBorrowing.reasons.${detail.reason || detail.message}`
    error.value = te(key) ? t(key) : t('ticketBorrowing.failure')
  } finally { if (version === generation) saving.value = false }
}
</script>
