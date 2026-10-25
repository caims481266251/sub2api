<template>
  <section class="space-y-3 rounded-lg border p-4 dark:border-dark-600" data-testid="borrow-statistics">
    <div class="flex flex-wrap items-center justify-between gap-3"><h3 class="text-sm font-medium">{{ t('ticketBorrowing.statistics.title') }}</h3><div class="flex gap-3"><button v-for="value in [2,15,60]" :key="value" class="text-sm" :class="minutes === value ? 'text-primary-600 font-medium' : 'text-gray-500'" :aria-pressed="minutes === value" @click="minutes = value">{{ t('ticketBorrowing.historyPage.lastMinutes', { count: value }) }}</button><button class="text-sm text-primary-600" :disabled="loading" @click="load">{{ t('ticketBorrowing.refresh') }}</button></div></div>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ t('ticketBorrowing.statistics.loadFailed') }}</p>
    <template v-if="stats">
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ new Date(stats.since).toLocaleTimeString() }} – {{ new Date(stats.until).toLocaleTimeString() }}</p>
      <dl class="grid grid-cols-2 gap-3 sm:grid-cols-4"><div v-for="key in verdicts" :key="key"><dt><BorrowTag kind="statistic" :value="key" /></dt><dd class="mt-1 font-medium tabular-nums">{{ stats[key] }}</dd></div></dl>
      <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statistics.counting') }}</p>
      <p v-if="observation" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statistics.availability', { seconds: Math.floor(observation.missingMS / 1000), sampled: Math.floor(observation.sampledMS / 1000) }) }}</p>
      <p v-if="stats.shared_target_count > 1" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statistics.sharedIdentity', { count: stats.shared_target_count, inflight: stats.shared_target_inflight }) }}</p>
      <p v-if="stats.last_success_at" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statistics.lastSuccess') }} {{ new Date(stats.last_success_at).toLocaleString() }}</p>
      <div v-for="phase in probePhases" :key="phase.name" class="rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800" :data-phase-statistics="phase.name">
        <p class="font-medium">{{ t(`ticketBorrowing.diagnostics.phases.${phase.name}`) }}</p>
        <p class="mt-1">{{ t('ticketBorrowing.diagnostics.phaseSummary', { requests: phase.value.requests, retries: phase.value.retries, samples: phase.value.diagnostic_samples, duration: duration(phase.value.average_ms), timeouts: phase.value.timeouts }) }}</p>
        <p v-for="(count, errorClass) in phase.value.error_classes" :key="errorClass">{{ diagnosticLabel('errors', String(errorClass)) }}: {{ count }}</p>
        <p v-for="(count, cause) in phase.value.cancellations" :key="cause">{{ diagnosticLabel('causes', String(cause)) }}: {{ count }}</p>
        <p v-for="(count, stage) in phase.value.timeout_stages" :key="stage">{{ t('ticketBorrowing.diagnostics.timeoutStage') }} · {{ diagnosticLabel('stages', String(stage)) }}: {{ count }}</p>
      </div>
      <p v-if="probePhases.length" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.diagnostics.statisticsHint') }}</p>
      <h4 class="pt-2 text-sm font-medium">{{ t('ticketBorrowing.statistics.businessTitle') }}</h4>
      <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statistics.businessScope') }}</p>
      <div v-for="item in stats.routes" :key="item.route_id" class="space-y-2 rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800">
        <button class="break-all font-mono text-primary-600" @click="$emit('records', item.route_id)">{{ item.gateway }} · {{ item.route_id }}</button>
        <div class="flex flex-wrap items-center gap-x-3 gap-y-2"><span>{{ t('ticketBorrowing.tags.requests') }} {{ item.requests }}</span><span class="inline-flex items-center gap-1"><BorrowTag kind="terminal" value="completed" />{{ item.completed }}</span><span class="inline-flex items-center gap-1"><BorrowTag kind="state" value="interrupted" />{{ item.interrupted }}</span><span class="inline-flex items-center gap-1"><BorrowTag kind="state" value="unconfirmed" />{{ item.unconfirmed }}</span></div>
        <p>{{ t('ticketBorrowing.statistics.visible', { time: duration(item.visible_average_ms), count: item.visible_samples }) }}</p>
        <p>{{ t('ticketBorrowing.statistics.semantic', { time: duration(item.semantic_average_ms), count: item.semantic_samples }) }}</p>
        <p>{{ t('ticketBorrowing.statistics.age', { time: duration(item.average_age_ms) }) }}</p>
        <button class="text-primary-600" :disabled="timingLoading" @click="loadTiming(item.latest_usage_id)">{{ t('ticketBorrowing.statistics.lastBusiness') }} #{{ item.latest_usage_id }}</button>
      </div>
      <p v-if="!stats.routes.length" class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statistics.noBusiness') }}</p>
      <details v-if="timings.length" open class="space-y-2 rounded-lg border p-3 dark:border-dark-600"><summary class="cursor-pointer text-sm">{{ t('ticketBorrowing.statistics.lastBusiness') }}</summary><div v-for="trace in timings" :key="trace.trace_id" class="mt-2 text-xs"><p class="break-all font-mono">{{ trace.trace_id }}</p><div class="mt-1 flex flex-wrap items-center gap-2"><span>{{ new Date(trace.started_at).toLocaleString() }}</span><BorrowTag kind="terminal" :value="trace.terminal || trace.outcome" /><span>{{ duration(trace.total_ms) }}</span></div><p>{{ t('ticketBorrowing.statistics.visible', { time: duration(trace.events.first_visible), count: trace.events.first_visible == null ? 0 : 1 }) }}</p></div></details>
    </template>
    <p v-else-if="loading" class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.loading') }}</p>
  </section>
</template>
<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ticketBorrowing, type BorrowStatistics } from '@/api/admin/ticketBorrowing'
import { getUsageTiming, type RequestTiming } from '@/api/admin/usageTiming'
import type { BorrowAvailability } from '@/composables/borrowAvailability'
const props = defineProps<{ targetId: number; observation?: BorrowAvailability }>()
defineEmits<{ records: [routeId: string] }>()
const { t, te } = useI18n()
const diagnosticLabel = (group: string, value: string) => {
 const key = `ticketBorrowing.diagnostics.${group}.${value}`
 return te(key) ? t(key) : value
}
const minutes = ref(15), stats = ref<BorrowStatistics>(), loading = ref(false), error = ref(false), timingLoading = ref(false), timings = ref<RequestTiming[]>([])
const probePhases = computed(() => (['mint', 'continue'] as const).flatMap(name => stats.value?.[name] ? [{ name, value: stats.value[name]! }] : []))
const verdicts = ['candidates','gateways','source_failed','inconclusive','route_changed','prioritized','rechecked','passed','published','ticket_changed','timeout','cancelled','other','skipped','inflight'] as const
let generation = 0, timingGeneration = 0
const duration = (ms?: number | null) => ms == null ? '—' : `${(ms/1000).toFixed(2)} s`
async function load() {
 const version = ++generation
 loading.value = true; error.value = false
 try { const value = await ticketBorrowing.statistics(props.targetId, minutes.value); if (version === generation) stats.value = value }
 catch { if (version === generation) error.value = true }
 finally { if (version === generation) loading.value = false }
}
async function loadTiming(id: number) {
 const version = ++timingGeneration; timingLoading.value = true
 try { const value = await getUsageTiming(id); if (version === timingGeneration) timings.value = value.traces }
 catch { if (version === timingGeneration) error.value = true }
 finally { if (version === timingGeneration) timingLoading.value = false }
}
watch([() => props.targetId, minutes], () => { stats.value = undefined; timings.value = []; timingGeneration++; timingLoading.value = false; void load() }, { immediate: true })
onBeforeUnmount(() => { generation++; timingGeneration++ })
</script>
