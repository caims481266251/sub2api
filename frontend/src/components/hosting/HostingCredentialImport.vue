<template>
  <div class="space-y-4" data-testid="hosting-credential-import">
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-dark-300">{{ t('hosting.importDescription') }}</p>
      <div class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-600 dark:border-amber-800 dark:bg-amber-900/20 dark:text-amber-400">{{ t(replaceId ? 'hosting.importReauthorizeHint' : 'hosting.importBatchHint', { seats: availableSeats }) }}</div>
      <div class="flex gap-2" :aria-label="t('hosting.importSource')">
        <button v-for="option in sourceOptions" :key="option.value" type="button" class="rounded-lg px-3 py-2 text-sm font-medium transition-colors" :class="source === option.value ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : 'text-gray-500 hover:bg-gray-100 dark:hover:bg-dark-700'" :data-testid="`hosting-import-${option.value}`" :aria-pressed="source === option.value" :disabled="importing || items.length > 0" @click="source = option.value">{{ option.label }}</button>
      </div>
      <div v-if="source === 'paste'" class="mb-4">
        <label for="hosting-import-json" class="mb-2 flex items-center gap-2 text-sm font-semibold text-gray-700 dark:text-gray-300">
          <Icon name="key" size="sm" class="text-blue-500" />
          {{ t('hosting.importJSONLabel') }}
        </label>
        <textarea
          id="hosting-import-json"
          v-model="text"
          rows="8"
          class="input w-full resize-y font-mono text-sm"
          :placeholder="t('hosting.importJSONPlaceholder')"
          :disabled="importing || items.length > 0"
          autocomplete="off"
          spellcheck="false"
        />
      </div>
      <JsonFilePicker
        v-else
        id="hosting-file"
        class="mb-4"
        :label="t('admin.accounts.dataImportFile')"
        :empty-label="t('admin.accounts.dataImportSelectFile')"
        :selected-label="files.length > 1 ? t('hosting.importSelectedFiles', { count: files.length }) : ''"
        :files="files"
        :multiple="!replaceId"
        :disabled="importing || items.length > 0"
        @select="selectFiles"
      />
      <div v-if="error" role="alert" class="mb-4 rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-700 dark:bg-red-900/30">
        <p class="whitespace-pre-line text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      </div>
      <button
        v-if="!hideActions && canRetryOrSubmit"
        type="button"
        class="btn btn-primary min-h-11 w-full"
        data-testid="hosting-import-submit"
        :disabled="importing || (!items.length && !hasInput)"
        @click="startImport"
      >
        <svg v-if="importing" class="-ml-1 mr-2 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
          <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
          <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z" />
        </svg>
        <Icon v-else name="sparkles" size="sm" class="mr-2" />
        {{ t(importing ? 'hosting.importing' : failedCount ? 'hosting.importRetryFailed' : replaceId ? 'hosting.importAndUpdate' : 'hosting.importAccount') }}
      </button>
    </div>
    <section v-if="items.length" class="space-y-2 rounded-xl border border-gray-200 p-4 dark:border-dark-700" data-testid="hosting-import-result" aria-live="polite">
      <h3 class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.accounts.dataImportResult') }}</h3>
      <p class="text-sm text-gray-700 dark:text-dark-300">{{ t('hosting.importSummary', { succeeded: successCount, failed: failedCount, total: items.length }) }}</p>
      <p v-if="importing" role="status" class="text-xs text-blue-600 dark:text-blue-400">{{ t('hosting.importProgress', { done: completedCount, total: items.length }) }}</p>
      <ul class="max-h-48 space-y-2 overflow-auto rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800">
        <li v-for="(item, index) in items" :key="index">
          <div class="flex flex-wrap justify-between gap-2"><span class="break-all">{{ item.name }} · {{ item.source }}</span><span :class="item.state === 'succeeded' ? 'text-green-600 dark:text-green-400' : item.state === 'failed' ? 'text-red-600 dark:text-red-400' : 'text-gray-500'">{{ t(`hosting.importStates.${item.state}`) }}</span></div>
          <p v-if="item.error" class="mt-1 whitespace-pre-wrap break-words text-red-600 dark:text-red-400">{{ item.error }}</p>
        </li>
      </ul>
      <p v-if="successCount && failedCount" class="input-hint">{{ t('hosting.importPartialHint') }}</p>
      <button v-if="!importing" type="button" class="btn btn-secondary min-h-11" @click="reset">{{ t('hosting.importNewBatch') }}</button>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import JsonFilePicker from '@/components/common/JsonFilePicker.vue'
import { hostingAPI, hostingErrorMessage, type HostedAccountSettings } from '@/api/hosting'
import { HostingImportInputError, parseHostingImport, readHostingImportFile } from '@/utils/hostingImport'
import { useAppStore } from '@/stores/app'

const props = withDefaults(defineProps<{ settings: HostedAccountSettings; replaceId?: number; availableSeats?: number; hideActions?: boolean }>(), { replaceId: 0, availableSeats: 0, hideActions: false })
const emit = defineEmits<{ busy: [value: boolean]; imported: [] }>()
const { t } = useI18n()
const app = useAppStore()
const source = ref<'paste' | 'files'>('files')
const text = ref(''), files = ref<File[]>([]), error = ref(''), importing = ref(false)
interface ImportItem { name: string; source: string; content: string; state: 'pending' | 'running' | 'succeeded' | 'failed'; error: string }
const items = ref<ImportItem[]>([])
const sourceOptions = computed(() => [{ value: 'files' as const, label: t('hosting.importJSONFiles') }, { value: 'paste' as const, label: t('hosting.importPasteJSON') }])
const hasInput = computed(() => source.value === 'paste' ? !!text.value.trim() : files.value.length > 0)
const successCount = computed(() => items.value.filter(item => item.state === 'succeeded').length)
const failedCount = computed(() => items.value.filter(item => item.state === 'failed').length)
const completedCount = computed(() => successCount.value + failedCount.value)
const canRetryOrSubmit = computed(() => !items.value.length || failedCount.value > 0 || importing.value)
const canSubmit = computed(() => !importing.value && (failedCount.value > 0 || (!items.value.length && hasInput.value)))
const submitLabel = computed(() => t(importing.value ? 'hosting.importing' : failedCount.value ? 'hosting.importRetryFailed' : 'admin.accounts.dataImportButton'))
defineExpose({ startImport, canSubmit, canRetryOrSubmit, submitLabel, hasResults: computed(() => items.value.length > 0) })

function selectFiles(selected: File[]) {
  if (importing.value || items.value.length) return
  error.value = ''
  files.value = []
  if (selected.some(file => !file.name.toLowerCase().endsWith('.json') && file.type !== 'application/json')) {
    error.value = t('hosting.invalidFile'); return
  }
  files.value = selected
}
function inputError(error: unknown) {
  return error instanceof HostingImportInputError ? t(error.translationKey) : t('hosting.importReadFailed')
}
async function prepareItems(): Promise<ImportItem[]> {
  const entries: { content: string; source: string; name?: string }[] = []
  if (source.value === 'paste') {
    for (const [index, entry] of parseHostingImport(text.value).entries()) {
      entries.push({ ...entry, source: t('hosting.importPastedItem', { index: index + 1 }) })
    }
  } else {
    for (const file of files.value) {
      try {
        const contents = parseHostingImport(await readHostingImportFile(file))
        entries.push(...contents.map((entry, index) => ({ ...entry, source: contents.length > 1 ? `${file.name} #${index + 1}` : file.name })))
      } catch (e) {
        throw new Error(t('hosting.importFileError', { name: file.name, message: inputError(e) }))
      }
    }
  }
  if (!entries.length) throw new HostingImportInputError('hosting.importEmpty')
  if (props.replaceId && entries.length !== 1) throw new HostingImportInputError('hosting.importSingleReauthorization')
  return entries.map((entry, index) => ({ ...entry, name: !props.replaceId && props.settings.name === 'Codex' && entry.name ? entry.name : entries.length === 1 ? props.settings.name : `${props.settings.name.slice(0, 90)} ${index + 1}`, state: 'pending', error: '' }))
}
async function startImport() {
  if (importing.value || (!items.value.length && !hasInput.value)) return
  importing.value = true; emit('busy', true); error.value = ''
  let saved = 0
  try {
    if (!items.value.length) {
      try { items.value = await prepareItems() }
      catch (e) { error.value = e instanceof HostingImportInputError ? t(e.translationKey) : hostingErrorMessage(e, t('hosting.importReadFailed')); return }
    }
    const settings: HostedAccountSettings = JSON.parse(JSON.stringify(props.settings))
    for (const item of items.value) {
      if (item.state === 'succeeded') continue
      item.state = 'running'; item.error = ''
      try {
        await hostingAPI.import(item.content, item.name, props.replaceId, settings.hosting_pool_id ?? 0, { ...settings, name: item.name })
        item.state = 'succeeded'; item.content = ''; saved += 1
      } catch (e) {
        item.state = 'failed'; item.error = hostingErrorMessage(e, t('hosting.importFailed'))
      }
    }
    text.value = ''; files.value = []
    if (failedCount.value) app.showError(t(successCount.value ? 'hosting.importPartial' : 'hosting.importFailed'))
    else app.showSuccess(t('hosting.imported'))
  } finally {
    importing.value = false; emit('busy', false)
    if (saved) emit('imported')
  }
}
function reset() {
  if (importing.value) return
  text.value = ''; files.value = []; error.value = ''; items.value = []
}
onBeforeUnmount(() => { text.value = ''; files.value = []; items.value = [] })
</script>
