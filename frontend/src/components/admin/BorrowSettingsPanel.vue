<template>
  <form class="min-w-0 space-y-4" data-testid="borrow-settings" @submit.prevent="$emit('save')">
    <div class="card flex flex-wrap items-center justify-between gap-3 px-4 py-3">
      <div><h2 class="font-semibold">{{ t('ticketBorrowing.settings') }}</h2><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.intro') }}</p></div>
      <span class="rounded-lg px-3 py-1 text-xs font-medium" :class="saved?.running ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'" data-testid="saved-running">{{ t('ticketBorrowing.policyPage.liveState') }} · {{ t(!saved ? 'ticketBorrowing.statusUnknown' : saved.running ? 'ticketBorrowing.running' : 'ticketBorrowing.paused') }}</span>
    </div>
    <fieldset :disabled="busy || !saved" class="grid min-w-0 gap-4 lg:grid-cols-2">
      <legend class="sr-only">{{ t('ticketBorrowing.settings') }}</legend>
      <section class="card min-w-0 overflow-hidden">
        <header class="border-b border-gray-100 px-5 py-4 dark:border-dark-700"><h3 class="text-sm font-semibold">{{ t('ticketBorrowing.replenishment.title') }}</h3><p class="mt-1 text-xs text-gray-500">{{ t('ticketBorrowing.replenishment.hint') }}</p></header>
        <div class="space-y-4 p-5">
          <div class="flex items-center justify-between gap-3"><label for="replenishment-enabled" class="text-sm font-medium">{{ t('ticketBorrowing.replenishment.enabled') }}</label><Toggle id="replenishment-enabled" v-model="replenishmentEnabled" data-testid="replenishment-enabled" :aria-label="t('ticketBorrowing.replenishment.enabled')" /></div>
          <details class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900/40" data-testid="replenishment-advanced"><summary class="cursor-pointer text-sm font-medium">{{ t('ticketBorrowing.replenishment.advanced') }}</summary>
          <div class="mt-3 grid gap-3 sm:grid-cols-2">
            <div><label for="replenishment-inventory" class="input-label">{{ t('ticketBorrowing.replenishment.inventory') }}</label><input id="replenishment-inventory" v-model.number="replenishmentInventory" type="number" min="1" max="4" step="1" class="input" /></div>
            <div><label for="replenishment-interval" class="input-label">{{ t('ticketBorrowing.replenishment.interval') }}</label><input id="replenishment-interval" v-model.number="replenishmentInterval" type="number" min="1" max="60" step="1" class="input" /></div>
            <div><label for="replenishment-hold" class="input-label">{{ t('ticketBorrowing.replenishment.hold') }}</label><input id="replenishment-hold" v-model.number="replenishmentHold" type="number" min="1" max="150" step="1" class="input" /></div>
          </div>
            <div class="mt-3"><label for="replenishment-maintenance" class="input-label">{{ t('ticketBorrowing.replenishment.maintenance') }}</label><input id="replenishment-maintenance" v-model.number="replenishmentMaintenance" type="number" min="10" max="300" step="1" class="input" /></div>
          </details>
          <p v-if="!replenishmentValid" class="text-xs text-red-600" role="alert">{{ t('ticketBorrowing.replenishment.invalid') }}</p>
        </div>
      </section>
      <section class="card min-w-0 overflow-hidden">
        <header class="border-b border-gray-100 px-5 py-4 dark:border-dark-700"><h3 class="text-sm font-semibold">{{ t('ticketBorrowing.policyPage.acquisition') }}</h3><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.acquisitionHint') }}</p></header>
        <div class="space-y-5 p-5">
          <div class="rounded-xl border border-gray-200 p-3 dark:border-dark-600">
            <label class="flex cursor-pointer items-center justify-between gap-3 text-sm font-medium"><span>{{ t('ticketBorrowing.background') }}</span><span class="flex items-center gap-2 text-xs"><span>{{ t(model.running ? 'ticketBorrowing.enabled' : 'ticketBorrowing.disabled') }}</span><input v-model="running" type="checkbox" class="h-4 w-4 accent-primary-600" data-testid="background-running" /></span></label>
            <p class="mt-2 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.backgroundHint') }}</p>
          </div>
          <div>
            <label for="borrow-ttl" class="input-label">{{ t('ticketBorrowing.policyPage.ttlLabel') }}</label>
            <div class="relative max-w-xs"><input id="borrow-ttl" v-model.number="ttl" type="number" min="30" max="240" step="1" class="input pr-12 tabular-nums" :class="{ 'input-error': !ttlValid }" :aria-invalid="!ttlValid" aria-describedby="borrow-ttl-hint" required data-testid="ttl" /><span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.secondsUnit') }}</span></div>
            <p id="borrow-ttl-hint" class="mt-2 text-xs leading-5" :class="ttlValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(ttlValid ? 'ticketBorrowing.ttlHint' : 'ticketBorrowing.policyPage.ttlError') }}</p>
          </div>
          <div>
            <label for="borrow-min-route-remaining" class="input-label">{{ t('ticketBorrowing.policyPage.minRouteRemainingLabel') }}</label>
            <div class="relative max-w-xs"><input id="borrow-min-route-remaining" v-model.number="minRouteRemaining" type="number" min="0" max="240" step="1" class="input pr-12 tabular-nums" :class="{ 'input-error': !minRouteRemainingValid }" :aria-invalid="!minRouteRemainingValid" aria-describedby="borrow-min-route-remaining-hint" required data-testid="min-route-remaining" /><span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.secondsUnit') }}</span></div>
            <p id="borrow-min-route-remaining-hint" class="mt-2 text-xs leading-5" :class="minRouteRemainingValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(minRouteRemainingValid ? 'ticketBorrowing.policyPage.minRouteRemainingHint' : 'ticketBorrowing.policyPage.minRouteRemainingError') }}</p>
            <p v-if="minRouteRemainingValid && minRouteRemaining >= ttl" class="mt-2 text-xs leading-5 text-amber-700 dark:text-amber-300" role="status">{{ t('ticketBorrowing.policyPage.minRouteRemainingWarning') }}</p>
          </div>
          <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900/40"><p class="text-xs leading-5 text-gray-600 dark:text-gray-400">{{ t('ticketBorrowing.expiryOnlyHint') }}</p></div>
        </div>
      </section>
      <section class="card min-w-0 overflow-hidden">
        <header class="border-b border-gray-100 px-5 py-4 dark:border-dark-700"><h3 class="text-sm font-semibold">{{ t('ticketBorrowing.requestBudgetTitle') }}</h3><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.budgetHint') }}</p></header>
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
        </div>
      </section>
      <section class="card min-w-0 overflow-hidden lg:col-span-2">
        <header class="border-b border-gray-100 px-5 py-4 dark:border-dark-700"><h3 class="text-sm font-semibold">{{ t('ticketBorrowing.policyPage.globalProbeTitle') }}</h3><p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.policyPage.globalProbeHint') }}</p></header>
        <div class="space-y-4 p-5">
          <div class="grid gap-4 sm:grid-cols-2">
            <div><label for="borrow-global-probe-concurrency" class="input-label">{{ t('ticketBorrowing.probePolicy.concurrency') }}</label><input id="borrow-global-probe-concurrency" v-model.number="globalProbeConcurrency" type="number" min="1" max="32" step="1" class="input tabular-nums" :class="{ 'input-error': !globalProbeValid }" :aria-invalid="!globalProbeValid" aria-describedby="borrow-global-probe-hint" required data-testid="global-probe-concurrency" /><p id="borrow-global-probe-hint" class="mt-2 text-xs leading-5" :class="globalProbeValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(globalProbeValid ? 'ticketBorrowing.probePolicy.concurrencyHint' : 'ticketBorrowing.probePolicy.concurrencyError') }}</p></div>
            <div><label for="borrow-global-renew" class="input-label">{{ t('ticketBorrowing.probePolicy.renewBefore') }}</label><div class="relative"><input id="borrow-global-renew" v-model.number="globalRenewBefore" type="number" min="0" max="240" step="1" class="input pr-12 tabular-nums" :class="{ 'input-error': !globalRenewValid }" :aria-invalid="!globalRenewValid" aria-describedby="borrow-global-renew-hint" required data-testid="global-renew-before" /><span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.secondsUnit') }}</span></div><p id="borrow-global-renew-hint" class="mt-2 text-xs leading-5" :class="globalRenewValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(globalRenewValid ? 'ticketBorrowing.probePolicy.renewHint' : 'ticketBorrowing.probePolicy.renewError') }}</p></div>
          </div>
          <div class="max-w-md">
            <label for="borrow-continue-timeout" class="input-label">{{ t('ticketBorrowing.probePolicy.continueTimeout') }}</label>
            <div class="relative max-w-xs"><input id="borrow-continue-timeout" v-model.number="continueTimeout" type="number" min="1" max="45" step="1" class="input pr-12 tabular-nums" :class="{ 'input-error': !continueTimeoutValid }" :aria-invalid="!continueTimeoutValid" aria-describedby="borrow-continue-timeout-hint" required data-testid="continue-timeout" /><span class="pointer-events-none absolute inset-y-0 right-4 flex items-center text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.secondsUnit') }}</span></div>
            <p id="borrow-continue-timeout-hint" class="mt-2 text-xs leading-5" :class="continueTimeoutValid ? 'text-gray-500 dark:text-gray-400' : 'text-red-600 dark:text-red-400'">{{ t(continueTimeoutValid ? 'ticketBorrowing.probePolicy.continueTimeoutHint' : 'ticketBorrowing.probePolicy.continueTimeoutError') }}</p>
          </div>
          <p v-if="globalRenewValid && minRouteRemainingValid && globalRenewBefore <= minRouteRemaining" class="text-xs leading-5 text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.policyPage.renewWindowHint') }}</p>
          <button type="button" class="btn btn-secondary btn-sm" @click="$emit('targets')">{{ t('ticketBorrowing.policyPage.targetPolicies') }} →</button>
        </div>
      </section>
    </fieldset>
    <div class="card flex flex-wrap items-center justify-between gap-3 p-4" :class="{ 'sticky bottom-3 z-10 shadow-lg': dirty || conflict }" data-testid="settings-save-bar">
      <div class="min-w-0"><p v-if="conflict" role="alert" class="text-sm text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.remoteChange') }}</p><p class="text-sm text-gray-500 dark:text-gray-400">{{ t(!saved ? 'ticketBorrowing.configurationNotLoaded' : dirty ? 'ticketBorrowing.unsavedHint' : 'ticketBorrowing.settingsSaved') }}</p></div>
      <div class="flex w-full flex-wrap gap-2 sm:w-auto"><button type="button" class="btn btn-secondary flex-1 sm:flex-none" :disabled="busy || (!dirty && !conflict)" data-testid="settings-revert" @click="$emit('revert')">{{ t('ticketBorrowing.revert') }}</button><button class="btn btn-primary flex-1 sm:flex-none" :disabled="busy || !saved || !dirty || conflict || !valid" data-testid="settings-save">{{ t(busy ? 'ticketBorrowing.loading' : 'ticketBorrowing.policyPage.save') }}</button></div>
    </div>
  </form>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { defaultBorrowReplenishment } from '@/api/admin/ticketBorrowing'
import type { BorrowSettings, BorrowRecoverySettings, BorrowReplenishmentSettings } from '@/api/admin/ticketBorrowing'
const model = defineModel<BorrowSettings & { recovery: BorrowRecoverySettings }>({ required: true })
const props = defineProps<{ saved?: BorrowSettings; busy: boolean; dirty: boolean; conflict: boolean; now: number }>()
defineEmits<{ save: []; revert: []; targets: [] }>()
const { t } = useI18n()
// Emit a fresh draft for every edit. A concurrent server refresh may have
// replaced the parent snapshot before the child receives its updated props.
const running = computed({ get: () => model.value.running, set: value => { model.value = { ...model.value, running: value } } })
const ttl = computed({ get: () => model.value.ttl_seconds, set: value => { model.value = { ...model.value, ttl_seconds: value } } })
const minRouteRemaining = computed({ get: () => model.value.min_route_remaining_seconds ?? 120, set: value => { model.value = { ...model.value, min_route_remaining_seconds: value } } })
const continueTimeout = computed({ get: () => model.value.continue_timeout_seconds ?? 7, set: value => { model.value = { ...model.value, continue_timeout_seconds: value } } })
const globalProbeConcurrency = computed({ get: () => model.value.probe_concurrency ?? 2, set: value => { model.value = { ...model.value, probe_concurrency: value } } })
const globalRenewBefore = computed({ get: () => model.value.renew_before_seconds ?? 180, set: value => { model.value = { ...model.value, renew_before_seconds: value } } })
const sourceBudget = computed({ get: () => model.value.recovery.source_requests_per_minute, set: value => { model.value = { ...model.value, recovery: { ...model.value.recovery, source_requests_per_minute: value } } } })
const probeBudget = computed({ get: () => model.value.recovery.probe_requests_per_minute, set: value => { model.value = { ...model.value, recovery: { ...model.value.recovery, probe_requests_per_minute: value } } } })

const replenishment = computed(() => model.value.replenishment ?? defaultBorrowReplenishment())
function updateReplenishment(patch: Partial<BorrowReplenishmentSettings>) { model.value = { ...model.value, replenishment: { ...replenishment.value, ...patch } } }
const replenishmentEnabled = computed({ get: () => replenishment.value.enabled, set: enabled => updateReplenishment({ enabled }) })
const replenishmentMaintenance = computed({ get: () => replenishment.value.maintenance_interval_seconds ?? Math.max(30, replenishment.value.interval_seconds), set: maintenance_interval_seconds => updateReplenishment({ maintenance_interval_seconds }) })
const replenishmentInventory = computed({ get: () => replenishment.value.inventory, set: inventory => updateReplenishment({ inventory }) })
const replenishmentInterval = computed({ get: () => replenishment.value.interval_seconds, set: interval_seconds => updateReplenishment({ interval_seconds }) })
const replenishmentHold = computed({ get: () => replenishment.value.hold_seconds, set: hold_seconds => updateReplenishment({ hold_seconds }) })
const replenishmentValid = computed(() => {
 const p = replenishment.value
 return inRange(p.inventory, 1, 4) && inRange(p.interval_seconds, 1, 60) && inRange(p.hold_seconds, 1, 150) && inRange(replenishmentMaintenance.value, Math.max(10, p.interval_seconds), 300)
})
const inRange = (value: unknown, min: number, max: number) => typeof value === 'number' && Number.isInteger(value) && value >= min && value <= max
const continueTimeoutValid = computed(() => inRange(continueTimeout.value, 1, 45))
const ttlValid = computed(() => inRange(model.value.ttl_seconds, 30, 240))
const minRouteRemainingValid = computed(() => inRange(minRouteRemaining.value, 0, 240))
const globalProbeValid = computed(() => inRange(globalProbeConcurrency.value, 1, 32))
const globalRenewValid = computed(() => inRange(globalRenewBefore.value, 0, 240))
const sourceValid = computed(() => inRange(model.value.recovery.source_requests_per_minute, 1, 120))
const probeValid = computed(() => inRange(model.value.recovery.probe_requests_per_minute, 2, 60))
const valid = computed(() => replenishmentValid.value && continueTimeoutValid.value && ttlValid.value && minRouteRemainingValid.value && globalProbeValid.value && globalRenewValid.value && sourceValid.value && probeValid.value)
const waitSeconds = computed(() => props.saved?.source_budget?.available_at ? Math.max(0, Math.ceil((Date.parse(props.saved.source_budget.available_at)-props.now)/1000)) : 0)
const budgetPercent = computed(() => { const budget = props.saved?.source_budget; return budget && budget.limit > 0 ? Math.min(100, Math.max(0, budget.used / budget.limit * 100)) : 0 })
</script>
