<template>
  <BaseDialog :show="show" :title="t('admin.accounts.activity.quality')" width="normal" :close-on-escape="!busy" @close="emit('close')">
    <div class="space-y-5">
      <div class="space-y-1">
        <p class="break-words text-sm font-medium text-gray-900 dark:text-gray-100">{{ account?.name }}</p>
        <p class="text-xs leading-5 text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.description') }}</p>
      </div>
      <div v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300">
        {{ error }} <button type="button" class="ml-2 underline" :disabled="busy || loading" @click="loadDetail()">{{ t('admin.accounts.activity.reload') }}</button>
      </div>
      <p v-if="loading && !detail" role="status" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <template v-if="detail">
        <div class="flex flex-wrap items-center gap-2">
          <span class="badge" :class="qualityBadgeClass(detail.state)" data-testid="quality-state">{{ t(`admin.accounts.activity.states.${detail.state}`) }}</span>
          <span class="text-xs text-gray-500 dark:text-dark-300">{{ t(detail.enabled ? 'admin.accounts.activity.autoOn' : 'admin.accounts.activity.autoOff') }}</span>
        </div>
        <p v-if="detail.pause_reason" class="rounded-lg bg-amber-50 p-3 text-xs leading-5 text-amber-800 dark:bg-amber-900/20 dark:text-amber-300">{{ detail.pause_reason }}</p>

        <form class="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-dark-600" @submit.prevent="save">
          <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-dark-200">
            <input v-model="draft.enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" :disabled="busy" data-testid="quality-auto-enabled" />
            {{ t('admin.accounts.activity.autoDetect') }}
          </label>
          <TestModelSelect id="account-quality-model" v-model="draft.model" :options="modelOptions" :label="t('admin.accounts.activity.model')"
            :hint="t('admin.accounts.activity.modelHint')" :disabled="busy" />
          <p v-if="modelsError" class="text-xs text-amber-700 dark:text-amber-400" role="status">
            {{ modelsError }} <button type="button" class="underline" :disabled="modelsLoading" @click="loadModels">{{ t('admin.accounts.activity.reloadModels') }}</button>
          </p>
          <label class="block space-y-1 text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.accounts.activity.interval') }}</span>
            <input v-model.number="draft.interval_minutes" type="number" min="1" max="1440" step="1" required class="input w-full" :disabled="busy" data-testid="quality-interval" />
          </label>
          <p class="text-xs leading-5 text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.intervalHint') }}</p>
          <button type="submit" class="btn btn-secondary btn-sm" :disabled="busy || !dirty || !valid" data-testid="save-quality">{{ t(saving ? 'admin.accounts.activity.saving' : 'common.save') }}</button>
        </form>

        <dl class="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm">
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.lastResult') }}</dt>
          <dd>{{ detail.latest_attempt ? t(`admin.accounts.activity.states.${detail.latest_attempt.state}`) : '—' }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.answer') }}</dt><dd class="break-words">{{ detail.latest_attempt?.answer || '—' }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.model') }}</dt><dd class="break-words">{{ detail.latest_attempt?.model || detail.model || '—' }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.checkedAt') }}</dt><dd>{{ detail.latest_attempt?.checked_at ? formatDateTime(detail.latest_attempt.checked_at) : '—' }}</dd>
          <dt class="text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.duration') }}</dt><dd>{{ activityDuration(detail.latest_attempt?.duration_ms) }}</dd>
          <template v-if="detail.enabled"><dt class="text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.nextCheck') }}</dt><dd>{{ detail.next_due_at ? formatDateTime(detail.next_due_at) : '—' }}</dd></template>
        </dl>
        <p v-if="detail.latest_attempt?.error" role="status" class="whitespace-pre-wrap break-words text-sm text-amber-700 dark:text-amber-400">{{ detail.latest_attempt.error }}</p>
        <div v-if="detail.last_valid" class="rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-700" data-testid="quality-last-valid">
          <p class="mb-1 text-xs text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.lastValid') }}</p>
          <p>{{ t(`admin.accounts.activity.states.${detail.last_valid.state}`) }} · {{ t('admin.accounts.activity.answer') }} {{ detail.last_valid.answer || '—' }}</p>
          <p class="mt-1 text-xs text-gray-400">{{ formatDateTime(detail.last_valid.checked_at) }}</p>
        </div>
        <details v-if="detail.latest_attempt?.text" class="rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600" data-testid="quality-raw-answer">
          <summary class="cursor-pointer font-medium">{{ t('admin.accounts.activity.rawAnswer') }}</summary>
          <pre class="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-words font-sans text-xs leading-5">{{ detail.latest_attempt.text }}</pre>
        </details>
      </template>
    </div>
    <template #footer>
      <div class="flex w-full flex-wrap items-center justify-between gap-3">
        <p class="max-w-72 text-xs text-gray-500 dark:text-dark-300">{{ t(dirty ? 'admin.accounts.activity.saveBeforeRun' : 'admin.accounts.activity.manualHint') }}</p>
        <button type="button" class="btn btn-primary btn-sm" :disabled="!detail || busy || dirty || detail.state === 'running' || detail.state === 'queued'" data-testid="run-quality" @click="run">
          {{ t(starting ? 'admin.accounts.activity.starting' : 'admin.accounts.activity.runNow') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import TestModelSelect from '@/components/admin/account/TestModelSelect.vue'
import { getAvailableModels } from '@/api/admin/accounts'
import { getAccountQuality, runAccountQuality, saveAccountQuality, type AccountQualityMonitor } from '@/api/admin/accountActivity'
import { activityDuration, qualityBadgeClass } from '@/utils/accountActivity'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{
  show: boolean
  account: { id: number; name: string } | null
  summary?: AccountQualityMonitor
}>()
const emit = defineEmits<{ (event: 'close'): void; (event: 'updated', quality: AccountQualityMonitor): void }>()
const { t } = useI18n()
const detail = ref<AccountQualityMonitor | null>(null)
const draft = reactive({ enabled: false, model: '', interval_minutes: 5 })
const loading = ref(false), saving = ref(false), starting = ref(false)
const error = ref(''), modelsError = ref(''), modelsLoading = ref(false)
const modelOptions = ref<Array<{ value: string; label: string }>>([])
const busy = computed(() => saving.value || starting.value)
const dirty = computed(() => !!detail.value && (draft.enabled !== detail.value.enabled || draft.model !== detail.value.model || draft.interval_minutes !== detail.value.interval_minutes))
const valid = computed(() => !!draft.model.trim() && Number.isInteger(draft.interval_minutes) && draft.interval_minutes >= 1 && draft.interval_minutes <= 1440)
// This picker hides known non-text families only. Account mappings and unknown
// provider aliases remain the backend's responsibility, including manual input.
const nonTextModelID = /^(?:(?:gpt-image|chatgpt-image|dall-e|whisper|tts|text-embedding|text-moderation|omni-moderation|sora)(?:-|$)|gpt-(?:\d+(?:\.\d+)?o?(?:-mini)?-)?(?:audio|realtime|transcribe|tts)(?:-|$))/i
let generation = 0, modelRequest = 0
let reader: AbortController | null = null

function receive(value: AccountQualityMonitor, reset = false) {
  const changedIdentity = detail.value != null && detail.value.scope !== value.scope
  const replaceDraft = reset || !dirty.value || changedIdentity
  detail.value = value
  if (replaceDraft) Object.assign(draft, { enabled: value.enabled, model: value.model, interval_minutes: value.interval_minutes })
  if (changedIdentity) error.value = t('admin.accounts.activity.accountChanged')
}

async function loadDetail(reset = false) {
  if (!props.show || !props.account) return
  reader?.abort()
  const current = new AbortController()
  reader = current
  const version = generation, accountID = props.account.id
  loading.value = true
  try {
    const value = await getAccountQuality(accountID, current.signal)
    if (generation !== version || current.signal.aborted) return
    error.value = ''
    receive(value, reset)
  } catch (e) {
    if (generation === version && !current.signal.aborted) error.value = extractApiErrorMessage(e) || t('admin.accounts.activity.loadFailed')
  } finally {
    if (reader === current) { reader = null; loading.value = false }
  }
}

async function loadModels() {
  if (!props.show || !props.account) return
  const version = generation, request = ++modelRequest
  modelsLoading.value = true
  try {
    const models = await getAvailableModels(props.account.id)
    if (generation !== version || request !== modelRequest) return
    modelOptions.value = models.filter(model => !nonTextModelID.test(model.id.trim())).map(model => ({ value: model.id, label: model.display_name && model.display_name !== model.id ? `${model.id} (${model.display_name})` : model.id }))
    modelsError.value = ''
  } catch (e) {
    if (generation === version && request === modelRequest) modelsError.value = extractApiErrorMessage(e) || t('admin.accounts.activity.modelsFailed')
  } finally {
    if (generation === version && request === modelRequest) modelsLoading.value = false
  }
}

async function save() {
  if (!props.account || !detail.value || busy.value || !valid.value) return
  const version = generation, id = props.account.id
  reader?.abort()
  saving.value = true
  error.value = ''
  try {
    const value = await saveAccountQuality(id, { scope: detail.value.scope, ...draft, model: draft.model.trim() })
    if (generation !== version) return
    receive(value, true)
    emit('updated', value)
  } catch (e) {
    if (generation === version) error.value = extractApiErrorMessage(e) || t('admin.accounts.activity.saveFailed')
  } finally { if (generation === version) saving.value = false }
}

async function run() {
  if (!props.account || !detail.value || busy.value || dirty.value || ['running', 'queued'].includes(detail.value.state)) return
  const version = generation, id = props.account.id
  reader?.abort()
  starting.value = true
  error.value = ''
  try {
    const value = await runAccountQuality(id, detail.value.scope)
    if (generation !== version) return
    receive(value)
    emit('updated', value)
  } catch (e) {
    if (generation === version) error.value = extractApiErrorMessage(e) || t('admin.accounts.activity.runFailed')
  } finally { if (generation === version) starting.value = false }
}

watch(() => `${props.show}:${props.account?.id ?? ''}`, () => {
  generation++
  reader?.abort()
  reader = null
  detail.value = null
  error.value = modelsError.value = ''
  modelOptions.value = []
  loading.value = saving.value = starting.value = modelsLoading.value = false
  if (props.show && props.account) { void loadDetail(true); void loadModels() }
}, { immediate: true })

watch(() => JSON.stringify(props.summary), () => {
  if (props.show && props.summary && props.summary.account_id === props.account?.id && !busy.value) void loadDetail()
})
onBeforeUnmount(() => { generation++; reader?.abort() })
</script>
