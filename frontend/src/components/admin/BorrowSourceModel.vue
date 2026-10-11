<template>
  <section class="mt-5 space-y-3 rounded-lg border p-4 dark:border-dark-600" data-testid="source-model-editor">
    <div class="flex flex-wrap items-center gap-2"><h3 class="text-sm font-medium">{{ t('ticketBorrowing.model') }}</h3><BorrowTag kind="state" :value="draft.inherit ? 'modelDefault' : 'modelOverride'" /><BorrowTag v-if="dirty" kind="state" value="unsaved" /></div>
    <fieldset :disabled="disabled || saving" class="space-y-3">
      <label class="flex items-center gap-2 text-sm"><input v-model="draft.inherit" type="checkbox" data-testid="source-model-default" />{{ t('ticketBorrowing.useDefault') }}</label>
      <input v-model="draft.model" :disabled="draft.inherit" class="input" maxlength="256" :placeholder="selection.default_model" :aria-label="t('ticketBorrowing.accountModel', { id: accountId })" data-testid="source-model-input" />
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.defaultModel') }}: {{ selection.default_model }}</p>
      <button class="btn btn-primary btn-sm" :disabled="!dirty || conflict || (!draft.inherit && !draft.model.trim())" data-testid="source-model-save" @click="save">{{ t('ticketBorrowing.management.saveAccountModel') }}</button>
      <button v-if="dirty || conflict" class="btn btn-ghost btn-sm" @click="reset">{{ t('ticketBorrowing.revert') }}</button>
    </fieldset>
    <p v-if="disabled" class="text-xs text-amber-700">{{ t('ticketBorrowing.management.modelScopeDraft') }}</p>
    <p v-if="conflict" class="text-sm text-amber-700" role="alert">{{ t('ticketBorrowing.remoteChange') }}</p>
    <p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ticketBorrowing, type BorrowSelection } from '@/api/admin/ticketBorrowing'
import { useBorrowEditorState, type BorrowEditorState } from '@/composables/useBorrowEditorState'
export interface SourceModelDraft { revision: number; inherit: boolean; model: string }
const props = defineProps<{ accountId: number; selection: BorrowSelection; disabled: boolean; editorState?: BorrowEditorState<SourceModelDraft> }>()
const emit = defineEmits<{ saved: [selection: BorrowSelection]; dirty: [value: boolean]; saving: [value: boolean]; editorState: [value: BorrowEditorState<SourceModelDraft>] }>()
const { t, te } = useI18n()
const value = (): SourceModelDraft => ({ revision: props.selection.revision, inherit: props.selection.model_overrides[props.accountId] === undefined, model: props.selection.model_overrides[props.accountId] ?? props.selection.default_model })
const { draft, baseline, dirty } = useBorrowEditorState(value(), props.editorState, state => emit('editorState', state))
const saving = ref(false), error = ref('')
const conflict = computed(() => draft.value.revision !== props.selection.revision)
function reset() { draft.value = value(); baseline.value = JSON.stringify(draft.value); error.value = '' }
watch(() => props.selection, () => { if (!dirty.value) reset() })
watch(dirty, value => emit('dirty', value), { immediate: true })
watch(saving, value => emit('saving', value), { flush: 'sync' })
let disposed = false
onBeforeUnmount(() => { disposed = true; emit('saving', false) })
async function save() {
  if (props.disabled || saving.value || conflict.value || !dirty.value || (!draft.value.inherit && !draft.value.model.trim())) return
  const input = { ...props.selection, model_overrides: { ...props.selection.model_overrides } }
  if (draft.value.inherit) delete input.model_overrides[props.accountId]
  else input.model_overrides[props.accountId] = draft.value.model.trim()
  saving.value = true; error.value = ''
  try {
    const saved = await ticketBorrowing.saveSelection(input)
    if (disposed) return
    draft.value = { ...draft.value, revision: saved.revision, model: saved.model_overrides[props.accountId] ?? saved.default_model }
    baseline.value = JSON.stringify(draft.value)
    emit('saved', saved)
  } catch (e) {
    if (disposed) return
    const detail = e as { reason?: string; message?: string }
    const key = `ticketBorrowing.reasons.${detail.reason || detail.message}`
    error.value = te(key) ? t(key) : t('ticketBorrowing.failure')
  } finally { if (!disposed) saving.value = false }
}
</script>
