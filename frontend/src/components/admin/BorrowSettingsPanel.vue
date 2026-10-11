<template>
  <form class="min-w-0 space-y-4" data-testid="borrow-settings" @submit.prevent="$emit('save')">
    <div class="card flex flex-wrap items-center justify-between gap-3 px-4 py-3">
      <div><h2 class="font-semibold">{{ t('ticketBorrowing.settings') }}</h2><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.intro') }}</p></div>
      <div class="flex flex-wrap items-center gap-2" data-testid="saved-running"><span class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.liveState') }}</span><BorrowTag kind="state" :value="!saved ? 'unknown' : saved.running ? 'running' : 'paused'" /><BorrowTag v-if="dirty" kind="state" value="unsaved" /></div>
    </div>
    <fieldset :disabled="busy || !saved" class="min-w-0 space-y-4">
      <legend class="sr-only">{{ t('ticketBorrowing.settings') }}</legend>
      <section class="card space-y-3 p-5" data-testid="settings-admission">
        <label for="borrow-admission-window" class="input-label">{{ t('ticketBorrowing.simplified.admissionWindow') }}</label>
        <div class="relative max-w-xs"><input id="borrow-admission-window" v-model.number="admissionWindow" type="number" min="1" max="240" step="1" class="input pr-12 tabular-nums" :aria-invalid="!admissionValid" aria-describedby="borrow-admission-hint" required data-testid="admission-window" /><span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500">{{ t('ticketBorrowing.secondsUnit') }}</span></div>
        <p id="borrow-admission-hint" class="text-xs leading-5" :class="admissionValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600'">{{ t(admissionValid ? 'ticketBorrowing.simplified.admissionHint' : 'ticketBorrowing.simplified.admissionError') }}</p>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.simplified.automaticRefill') }}</p>
      </section>
      <section class="card min-w-0 overflow-hidden" data-testid="settings-limits">
          <details class="rounded-lg bg-blue-50 p-3 text-sm dark:bg-blue-900/20" data-testid="global-policy-impact"><summary class="cursor-pointer">{{ t('ticketBorrowing.management.policyImpact', { inherited: targets?.filter(v => v.enabled && v.probe_policy?.use_global).length ?? 0, overridden: targets?.filter(v => v.enabled && !v.probe_policy?.use_global).length ?? 0 }) }}</summary><ul class="mt-2 space-y-1 text-xs"><li v-for="target in targets?.filter(v => v.enabled)" :key="target.account_id"><button type="button" class="text-primary-600 underline" @click="$emit('target', target.account_id)">{{ target.name }} #{{ target.account_id }}</button> <BorrowTag kind="state" :value="target.probe_policy?.use_global ? 'inherited' : 'overridden'" /></li></ul></details>
        <details data-testid="settings-advanced" :open="advancedOpen" @toggle="advancedOpen = ($event.target as HTMLDetailsElement).open">
          <summary class="cursor-pointer px-5 py-4"><span class="text-sm font-semibold">{{ t('ticketBorrowing.management.probeLimits') }}</span><span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.management.probeSummary', { source: sourceBudget, target: probeBudget, concurrency: globalProbeConcurrency, timeout: continueTimeout }) }}</span></summary>
        <div class="space-y-5 p-5">
          <div class="grid gap-4 sm:grid-cols-2">
            <div><label for="borrow-source-budget" class="input-label">{{ t('ticketBorrowing.policyPage.sourceBudgetLabel') }}</label><div class="relative"><input id="borrow-source-budget" v-model.number="sourceBudget" type="number" min="1" max="120" step="1" class="input pr-16 tabular-nums" :class="{ 'input-error': !sourceValid }" :aria-invalid="!sourceValid" aria-describedby="borrow-source-budget-hint" required data-testid="source-budget" /><span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.perMinute') }}</span></div><p id="borrow-source-budget-hint" class="mt-2 text-xs leading-5" :class="sourceValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(sourceValid ? 'ticketBorrowing.policyPage.sourceRange' : 'ticketBorrowing.policyPage.sourceError') }}</p></div>
            <div><label for="borrow-probe-budget" class="input-label">{{ t('ticketBorrowing.policyPage.targetBudgetLabel') }}</label><div class="relative"><input id="borrow-probe-budget" v-model.number="probeBudget" type="number" min="2" max="60" step="1" class="input pr-16 tabular-nums" :class="{ 'input-error': !probeValid }" :aria-invalid="!probeValid" aria-describedby="borrow-probe-budget-hint" required data-testid="probe-budget" /><span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.perMinute') }}</span></div><p id="borrow-probe-budget-hint" class="mt-2 text-xs leading-5" :class="probeValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(probeValid ? 'ticketBorrowing.policyPage.targetRange' : 'ticketBorrowing.policyPage.targetError') }}</p></div>
          </div>
          <div v-if="saved?.source_budget" class="rounded-xl border border-gray-200 p-3 dark:border-dark-600" data-testid="source-budget-status" role="status">
            <div class="flex flex-wrap items-center justify-between gap-2 text-xs"><span class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.sourceBudgetUsed') }}</span><strong class="tabular-nums">{{ saved.source_budget.used }} / {{ saved.source_budget.limit }}</strong></div>
            <div class="mt-3 h-1.5 overflow-hidden rounded-full bg-gray-100 dark:bg-dark-700" aria-hidden="true"><div class="h-full rounded-full bg-primary-500" :style="{ width: `${budgetPercent}%` }"></div></div>
            <p v-if="waitSeconds > 0" class="mt-2 text-xs text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.budgetWaitRemaining', { count: waitSeconds }) }}</p>
            <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.savedBudgetHint') }}</p>
          </div>
          <details class="rounded-xl bg-gray-50 p-3 text-xs text-gray-600 dark:bg-dark-900/40 dark:text-gray-400"><summary class="cursor-pointer font-medium">{{ t('ticketBorrowing.policyPage.budgetRules') }}</summary><p class="mt-2 leading-5">{{ t('ticketBorrowing.probeBudgetHint') }}</p></details>
        </div>        <div class="space-y-4 p-5">

          <div class="grid gap-4 sm:grid-cols-2">
            <div><label for="borrow-global-probe-concurrency" class="input-label">{{ t('ticketBorrowing.probePolicy.concurrency') }}</label><input id="borrow-global-probe-concurrency" v-model.number="globalProbeConcurrency" type="number" min="1" max="32" step="1" class="input tabular-nums" :class="{ 'input-error': !globalProbeValid }" :aria-invalid="!globalProbeValid" aria-describedby="borrow-global-probe-hint" required data-testid="global-probe-concurrency" /><p id="borrow-global-probe-hint" class="mt-2 text-xs leading-5" :class="globalProbeValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(globalProbeValid ? 'ticketBorrowing.probePolicy.concurrencyHint' : 'ticketBorrowing.probePolicy.concurrencyError') }}</p></div>
          </div>
          <div class="max-w-md">
            <label for="borrow-continue-timeout" class="input-label">{{ t('ticketBorrowing.probePolicy.continueTimeout') }}</label>
            <div class="relative max-w-xs"><input id="borrow-continue-timeout" v-model.number="continueTimeout" type="number" min="1" max="45" step="1" class="input pr-12 tabular-nums" :class="{ 'input-error': !continueTimeoutValid }" :aria-invalid="!continueTimeoutValid" aria-describedby="borrow-continue-timeout-hint" required data-testid="continue-timeout" /><span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.secondsUnit') }}</span></div>
            <p id="borrow-continue-timeout-hint" class="mt-2 text-xs leading-5" :class="continueTimeoutValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(continueTimeoutValid ? 'ticketBorrowing.probePolicy.continueTimeoutHint' : 'ticketBorrowing.probePolicy.continueTimeoutError') }}</p>
          </div>
          <button type="button" class="btn btn-secondary btn-sm" @click="$emit('targets')">{{ t('ticketBorrowing.policyPage.targetPolicies') }} →</button>
        </div>
        </details>
      </section>
    </fieldset>
    <section v-if="conflict && saved" class="rounded-lg border border-amber-300 p-4 text-sm dark:border-amber-700" data-testid="settings-conflict-diff">
      <p class="font-medium text-amber-800 dark:text-amber-300">{{ t('ticketBorrowing.remoteChange') }}</p>
      <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.management.conflictRevisions', { local: model.revision, server: saved.revision }) }}</p>
      <div v-if="differences.length" class="mt-3 overflow-x-auto"><table class="w-full text-left text-xs"><thead><tr><th class="pb-2">{{ t('ticketBorrowing.management.parameter') }}</th><th class="pb-2">{{ t('ticketBorrowing.management.localDraft') }}</th><th class="pb-2">{{ t('ticketBorrowing.management.serverValue') }}</th></tr></thead><tbody><tr v-for="item in differences" :key="item.label" class="border-t dark:border-dark-600"><td class="py-2 pr-3">{{ t(item.label) }}</td><td class="py-2 pr-3">{{ display(item.local) }}</td><td class="py-2">{{ display(item.server) }}</td></tr></tbody></table></div>
      <p v-else class="mt-2 text-xs">{{ t('ticketBorrowing.management.sameValuesConflict') }}</p>
    </section>
    <div class="card flex flex-wrap items-center justify-between gap-3 p-4" :class="{ 'sticky bottom-3 z-10 shadow-lg': dirty || conflict }" data-testid="settings-save-bar">
      <div class="min-w-0"><p v-if="conflict" role="alert" class="text-sm text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.remoteChange') }}</p><p class="text-sm text-gray-500 dark:text-gray-400">{{ t(!saved ? 'ticketBorrowing.configurationNotLoaded' : dirty ? 'ticketBorrowing.unsavedHint' : 'ticketBorrowing.settingsSaved') }}</p></div>
      <div class="flex w-full flex-wrap gap-2 sm:w-auto"><button type="button" class="btn btn-secondary flex-1 sm:flex-none" :disabled="busy || (!dirty && !conflict)" data-testid="settings-revert" @click="$emit('revert')">{{ t('ticketBorrowing.revert') }}</button><button class="btn btn-primary flex-1 sm:flex-none" :disabled="busy || !saved || !dirty || conflict || !valid" data-testid="settings-save">{{ t(busy ? 'ticketBorrowing.loading' : 'ticketBorrowing.policyPage.save') }}</button></div>
    </div>
  </form>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowSettings, BorrowRecoverySettings, BorrowTarget } from '@/api/admin/ticketBorrowing'
const model = defineModel<BorrowSettings & { recovery: BorrowRecoverySettings }>({ required: true })
const props = defineProps<{ saved?: BorrowSettings; busy: boolean; dirty: boolean; conflict: boolean; now: number; targets?: BorrowTarget[] }>()
defineEmits<{ save: []; revert: []; targets: []; target: [id: number] }>()
const { t } = useI18n()
// Emit a fresh draft for every edit. A concurrent server refresh may have
// replaced the parent snapshot before the child receives its updated props.
const admissionWindow = computed({ get: () => model.value.admission_window_seconds ?? Math.max(1, model.value.ttl_seconds - (model.value.min_route_remaining_seconds ?? 0)), set: value => { model.value = { ...model.value, admission_window_seconds: value } } })
const continueTimeout = computed({ get: () => model.value.continue_timeout_seconds ?? 7, set: value => { model.value = { ...model.value, continue_timeout_seconds: value } } })
const globalProbeConcurrency = computed({ get: () => model.value.probe_concurrency ?? 2, set: value => { model.value = { ...model.value, probe_concurrency: value } } })
const sourceBudget = computed({ get: () => model.value.recovery.source_requests_per_minute, set: value => { model.value = { ...model.value, recovery: { ...model.value.recovery, source_requests_per_minute: value } } } })
const probeBudget = computed({ get: () => model.value.recovery.probe_requests_per_minute, set: value => { model.value = { ...model.value, recovery: { ...model.value.recovery, probe_requests_per_minute: value } } } })

const inRange = (value: unknown, min: number, max: number) => typeof value === 'number' && Number.isInteger(value) && value >= min && value <= max
const continueTimeoutValid = computed(() => inRange(continueTimeout.value, 1, 45))
const admissionValid = computed(() => inRange(admissionWindow.value, 1, 240))
const globalProbeValid = computed(() => inRange(globalProbeConcurrency.value, 1, 32))
const sourceValid = computed(() => inRange(model.value.recovery.source_requests_per_minute, 1, 120))
const probeValid = computed(() => inRange(model.value.recovery.probe_requests_per_minute, 2, 60))
const advancedOpen = ref(false)
watch([sourceValid, probeValid, globalProbeValid, continueTimeoutValid], validity => {
  if (validity.some(value => !value)) advancedOpen.value = true
}, { immediate: true })
const valid = computed(() => continueTimeoutValid.value && admissionValid.value && globalProbeValid.value && sourceValid.value && probeValid.value)
const waitSeconds = computed(() => props.saved?.source_budget?.available_at ? Math.max(0, Math.ceil((Date.parse(props.saved.source_budget.available_at)-props.now)/1000)) : 0)
const budgetPercent = computed(() => { const budget = props.saved?.source_budget; return budget && budget.limit > 0 ? Math.min(100, Math.max(0, budget.used / budget.limit * 100)) : 0 })

const differenceFields: Array<[string, (value: BorrowSettings) => unknown]> = [
 ['ticketBorrowing.simplified.admissionWindow', v => v.admission_window_seconds],
 ['ticketBorrowing.background', v => v.running],
 ['ticketBorrowing.replenishment.title', v => v.replenishment?.enabled ?? false],
 ['ticketBorrowing.probePolicy.concurrency', v => v.probe_concurrency ?? 2],
 ['ticketBorrowing.probePolicy.continueTimeout', v => v.continue_timeout_seconds ?? 7],
 ['ticketBorrowing.policyPage.sourceBudgetLabel', v => v.recovery?.source_requests_per_minute ?? 12],
 ['ticketBorrowing.policyPage.targetBudgetLabel', v => v.recovery?.probe_requests_per_minute ?? 12],
]
const differences = computed(() => props.saved ? differenceFields.map(([label, get]) => ({ label, local: get(model.value), server: get(props.saved!) })).filter(item => item.local !== item.server) : [])
function display(value: unknown) { return typeof value === 'boolean' ? t(value ? 'ticketBorrowing.enabled' : 'ticketBorrowing.disabled') : String(value ?? '—') }
</script>
