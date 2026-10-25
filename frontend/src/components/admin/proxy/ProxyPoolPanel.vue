<template>
  <section class="proxy-pool-panel min-w-0 space-y-5" :aria-label="t('proxyPool.title')">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div class="min-w-0 flex-1"><h2 class="text-lg font-semibold">{{ t('proxyPool.title') }}</h2><p class="mt-1 max-w-3xl text-sm leading-6 text-gray-600 dark:text-gray-400">{{ t('proxyPool.description') }}</p></div>
      <button type="button" class="btn btn-secondary shrink-0" :disabled="busy" @click="reload">{{ t('common.refresh') }}</button>
    </header>
    <p v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20">{{ error }}</p>
    <p v-if="notice" role="status" class="text-sm text-emerald-600">{{ notice }}</p>
    <div v-if="state && !state.encryption_configured" class="card space-y-3 p-4">
      <p class="text-sm">{{ t('proxyPool.encryptionHint') }}</p>
      <button type="button" class="btn btn-secondary" :disabled="busy" @click="initializeEncryption">{{ t('proxyPool.initializeEncryption') }}</button>
    </div>
    <form class="min-w-0 space-y-4 rounded-xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-600 dark:bg-dark-900/30 sm:p-5" @submit.prevent="previewImport">
      <fieldset :disabled="busy || !state" class="min-w-0 space-y-3">
        <legend class="mb-2 text-sm font-semibold">{{ t('proxyPool.importTitle') }}</legend>
        <label class="block max-w-sm space-y-2"><span class="input-label">{{ t('proxyPool.mode') }}</span><select v-model="mode" class="input" data-testid="pool-mode"><option value="append">{{ t('proxyPool.append') }}</option><option value="replace">{{ t('proxyPool.replace') }}</option></select></label>
        <p class="text-sm text-gray-500">{{ t(mode === 'replace' ? 'proxyPool.replaceHint' : 'proxyPool.appendHint') }}</p>
        <label class="block space-y-2"><span class="input-label">{{ t('proxyPool.lines') }}</span><textarea v-model="text" rows="5" class="input min-h-32 resize-y font-mono text-xs leading-6" autocomplete="off" autocapitalize="off" :spellcheck="false" :placeholder="t('proxyPool.placeholder')" data-testid="pool-input" /></label>
        <p class="text-xs text-gray-500">{{ t('proxyPool.inputHint') }}</p>
        <div class="flex flex-col gap-2 sm:flex-row sm:flex-wrap"><button type="submit" class="btn btn-primary" :disabled="!text.trim()" data-testid="pool-preview">{{ t('proxyPool.preview') }}</button><button type="button" class="btn btn-ghost" :disabled="!text" @click="clearDraft">{{ t('proxyPool.clearDraft') }}</button></div>
      </fieldset>
      <div v-if="preview" class="space-y-3 rounded-lg border p-4 dark:border-dark-600" data-testid="pool-preview-result">
        <p class="font-medium">{{ t('proxyPool.previewSummary', { valid: preview.valid_count, duplicates: preview.duplicate_count, added: preview.added_count, retained: preview.retained_count, removed: preview.removed_count }) }}</p>
        <ul v-if="preview.issues.length" class="space-y-1 text-sm text-red-600"><li v-for="(issue, index) in preview.issues" :key="index">{{ issue.line ? t('proxyPool.lineNumber', { line: issue.line }) + ': ' : '' }}{{ t(`proxyPool.issues.${issue.reason}`) }}</li></ul>
        <p v-if="mode === 'replace'" class="text-sm text-amber-600">{{ t('proxyPool.replaceConfirmation', { count: preview.removed_count }) }}</p>
        <button type="button" class="btn" :class="mode === 'replace' ? 'btn-secondary' : 'btn-primary'" :disabled="busy || !preview.can_apply" data-testid="pool-apply" @click="applyImport">{{ t(mode === 'replace' ? 'proxyPool.applyReplace' : 'proxyPool.applyAppend') }}</button>
      </div>
    </form>
    <div v-if="state" class="min-w-0 overflow-hidden rounded-xl border border-gray-200 dark:border-dark-600">
      <div class="flex flex-wrap items-center justify-between gap-3 p-4"><p class="text-sm">{{ t('proxyPool.inventory', { count: state.entries.length, revision: state.revision }) }}</p><input v-model="search" class="input w-full sm:max-w-xs" :aria-label="t('proxyPool.search')" :placeholder="t('proxyPool.search')" /></div>
      <p class="px-4 pb-3 text-xs text-gray-500 dark:text-gray-400 sm:hidden">{{ t('proxyPool.scrollHint') }}</p>
      <div class="overflow-x-auto"><table class="w-full min-w-[540px] text-left text-sm"><thead><tr class="border-b dark:border-dark-600"><th class="p-3">ID</th><th class="p-3">{{ t('proxyPool.endpoint') }}</th><th class="p-3">{{ t('proxyPool.credentials') }}</th><th class="p-3">{{ t('proxyPool.status') }}</th><th class="p-3">{{ t('proxyPool.action') }}</th></tr></thead><tbody><tr v-for="entry in pageEntries" :key="entry.id" class="border-b transition-colors hover:bg-gray-50 dark:border-dark-600 dark:hover:bg-dark-900/40" :data-pool-entry="entry.id"><td class="whitespace-nowrap p-3 text-xs tabular-nums text-gray-500">#{{ entry.id }}</td><td class="min-w-56 break-all p-3 font-mono text-xs leading-5">{{ entry.protocol }}://{{ entry.host.includes(':') ? `[${entry.host}]` : entry.host }}:{{ entry.port }}</td><td class="whitespace-nowrap p-3 text-xs">{{ t(entry.authenticated ? 'proxyPool.encrypted' : 'proxyPool.noCredentials') }}</td><td class="whitespace-nowrap p-3"><span class="inline-flex items-center gap-1.5 rounded-full px-2 py-1 text-xs font-medium" :class="entry.enabled ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'"><span class="h-1.5 w-1.5 rounded-full bg-current" aria-hidden="true"></span>{{ t(entry.enabled ? 'proxyPool.enabled' : 'proxyPool.disabled') }}</span></td><td class="p-3"><button type="button" class="btn btn-secondary btn-sm whitespace-nowrap" :disabled="busy" @click="toggle(entry)">{{ t(entry.enabled ? 'proxyPool.disable' : 'proxyPool.enable') }}</button></td></tr><tr v-if="!pageEntries.length"><td colspan="5" class="p-6 text-center text-gray-500">{{ t('proxyPool.empty') }}</td></tr></tbody></table></div>
      <div v-if="filtered.length > pageSize" class="flex flex-wrap items-center justify-between gap-3 bg-gray-50/60 p-3 text-sm tabular-nums dark:bg-dark-900/30"><button type="button" class="btn btn-secondary" :disabled="page === 1" @click="page--">{{ t('pagination.previous') }}</button><span>{{ page }} / {{ Math.ceil(filtered.length / pageSize) }}</span><button type="button" class="btn btn-secondary" :disabled="page * pageSize >= filtered.length" @click="page++">{{ t('pagination.next') }}</button></div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { proxyPool as api, type ProxyPoolEntry, type ProxyPoolInput, type ProxyPoolPreview, type ProxyPoolState } from '@/api/admin/proxyPool'

const { t, te } = useI18n()
const state = ref<ProxyPoolState>(), preview = ref<ProxyPoolPreview>()
const text = ref(''), mode = ref<ProxyPoolInput['mode']>('append'), busy = ref(false), error = ref(''), notice = ref('')
const search = ref(''), page = ref(1), pageSize = 20
let disposed = false
const filtered = computed(() => (state.value?.entries ?? []).filter(e => `${e.id} ${e.protocol} ${e.host} ${e.port}`.toLowerCase().includes(search.value.trim().toLowerCase())))
const pageEntries = computed(() => filtered.value.slice((page.value - 1) * pageSize, page.value * pageSize))
watch([text, mode], () => { preview.value = undefined; notice.value = '' }, { flush: 'sync' })
watch([search, () => state.value?.revision], () => { page.value = 1 })
function fail(e: unknown) { const reason = (e as { reason?: string })?.reason; const key = `proxyPool.errors.${reason}`; error.value = reason && te(key) ? t(key) : t('proxyPool.errors.proxy_pool_unavailable'); preview.value = undefined }
async function action(fn: () => Promise<void>) {
  if (busy.value) return
  busy.value = true; error.value = ''
  try { await fn() } catch (e) { if (!disposed) fail(e) } finally { busy.value = false }
}
async function reload() { await action(async () => { const result = await api.state(); if (!disposed) { state.value = result; preview.value = undefined } }) }
async function initializeEncryption() { await action(async () => { const result = await api.initializeEncryption(); if (!disposed) { state.value = result; preview.value = undefined } }) }
function input(): ProxyPoolInput { return { revision: state.value!.revision, mode: mode.value, text: text.value } }
async function previewImport() {
  if (!state.value || !text.value.trim()) return
  const draft = input()
  await action(async () => { const result = await api.preview(draft); if (!disposed && text.value === draft.text && mode.value === draft.mode) preview.value = result })
}
async function applyImport() {
  if (!state.value || !preview.value?.can_apply || preview.value.revision !== state.value.revision) return
  const draft = input()
  await action(async () => { const result = await api.import(draft); if (!disposed) { state.value = result; clearDraft(); notice.value = t('proxyPool.saved') } })
}
function clearDraft() { text.value = ''; preview.value = undefined }
async function toggle(entry: ProxyPoolEntry) { if (!state.value) return; const revision = state.value.revision; await action(async () => { const result = await api.setEnabled(entry.id, revision, !entry.enabled); if (!disposed) { state.value = result; preview.value = undefined } }) }
function beforeUnload(event: BeforeUnloadEvent) { if (text.value) { event.preventDefault(); event.returnValue = '' } }
onBeforeRouteLeave(() => !text.value || window.confirm(t('proxyPool.leaveConfirm')))
onMounted(() => { window.addEventListener('beforeunload', beforeUnload); void reload() })
onBeforeUnmount(() => { disposed = true; text.value = ''; window.removeEventListener('beforeunload', beforeUnload) })
</script>

<style scoped>
.proxy-pool-panel .text-xs { @apply leading-5; }
.proxy-pool-panel .text-gray-500 { @apply dark:text-gray-400; }
</style>
