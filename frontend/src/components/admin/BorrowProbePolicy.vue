<template>
  <section class="min-w-0 rounded-xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-600 dark:bg-dark-900/30 sm:p-5" :data-probe-policy="target.account_id">
    <div class="flex flex-wrap items-center gap-2"><h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('ticketBorrowing.probePolicy.title') }}</h3><BorrowTag kind="state" :value="draft.use_global ? 'inherited' : 'overridden'" /><BorrowTag v-if="dirty" kind="state" value="unsaved" /></div>
    <p class="mt-1.5 max-w-3xl text-xs leading-5 text-gray-600 dark:text-gray-400">{{ t('ticketBorrowing.probePolicy.hint') }}</p>
    <label class="mt-4 flex items-center gap-2 text-sm"><input v-model="draft.use_global" type="checkbox" class="h-4 w-4 accent-primary-600" :disabled="saving || !loaded" data-testid="probe-use-global" />{{ t('ticketBorrowing.probePolicy.useGlobal') }}</label>
    <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t(draft.use_global ? 'ticketBorrowing.probePolicy.inheritedHint' : 'ticketBorrowing.probePolicy.overrideHint') }}</p>
    <div class="mt-4 grid max-w-2xl gap-4 sm:grid-cols-2">
      <div class="min-w-0">
        <label :for="`probe-concurrency-${target.account_id}`" class="input-label">{{ t('ticketBorrowing.probePolicy.concurrency') }}</label>
        <input :id="`probe-concurrency-${target.account_id}`" v-model.number="concurrency" type="number" min="1" max="32" step="1" class="input h-11 tabular-nums" :class="{ 'input-error': !concurrencyValid }" :disabled="saving || !loaded || draft.use_global" :aria-invalid="!concurrencyValid" :aria-describedby="`probe-concurrency-hint-${target.account_id}`" data-testid="probe-concurrency" />
        <p :id="`probe-concurrency-hint-${target.account_id}`" class="mt-1.5 text-xs leading-5" :class="concurrencyValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(concurrencyValid ? 'ticketBorrowing.probePolicy.concurrencyHint' : 'ticketBorrowing.probePolicy.concurrencyError') }}</p>
      </div>
    </div>
    <p v-if="conflict" role="alert" class="mt-3 text-sm text-amber-700 dark:text-amber-400">{{ t('ticketBorrowing.remoteChange') }}</p>
    <p v-if="error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <div class="mt-4 flex flex-wrap items-center gap-3 border-t border-gray-200 pt-4 dark:border-dark-600">
      <button type="button" class="btn w-full sm:w-auto" :class="dirty && valid && !conflict ? 'btn-primary' : 'btn-secondary'" :disabled="saving || !loaded || !dirty || !valid || conflict" data-testid="probe-policy-save" @click="save">{{ t(saving ? 'ticketBorrowing.loading' : 'ticketBorrowing.probePolicy.save') }}</button>
      <button v-if="dirty || conflict" type="button" class="btn btn-ghost w-full sm:w-auto" :disabled="saving" data-testid="probe-policy-revert" @click="reset">{{ t('ticketBorrowing.revert') }}</button>
      <BorrowTag v-if="saved && !dirty" kind="state" value="saved" />
    </div>
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useBorrowEditorState, type BorrowEditorState } from '@/composables/useBorrowEditorState'
import { useI18n } from 'vue-i18n'
import { ticketBorrowing, type BorrowTarget, type BorrowProbePolicy } from '@/api/admin/ticketBorrowing'
const props = defineProps<{ target: BorrowTarget; editorState?: BorrowEditorState<BorrowProbePolicy> }>()
const emit = defineEmits<{ saved: [target: BorrowTarget]; dirty: [value: boolean]; editorState: [value: BorrowEditorState<BorrowProbePolicy>]; saving: [value: boolean] }>()
const { t, te } = useI18n()
const value = (): BorrowProbePolicy => ({ revision: props.target.probe_policy?.revision ?? 0, concurrency: props.target.probe_policy?.concurrency ?? 2, renew_before_seconds: props.target.probe_policy?.renew_before_seconds ?? 180, use_global: props.target.probe_policy?.use_global ?? false })
const { draft, baseline, dirty } = useBorrowEditorState(value(), props.editorState, state => emit('editorState', state))
const saving = ref(false), error = ref(''), saved = ref(false)
const concurrency = computed({ get: () => draft.value.use_global ? props.target.probe_policy?.global_concurrency ?? draft.value.concurrency : draft.value.concurrency, set: value => { draft.value.concurrency = value } })
let generation = 0
const loaded = computed(() => (props.target.probe_policy?.revision ?? 0) > 0)
const conflict = computed(() => draft.value.revision !== (props.target.probe_policy?.revision ?? 0))
const concurrencyValid = computed(() => draft.value.use_global || Number.isInteger(draft.value.concurrency) && draft.value.concurrency >= 1 && draft.value.concurrency <= 32)
const valid = computed(() => concurrencyValid.value)
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
