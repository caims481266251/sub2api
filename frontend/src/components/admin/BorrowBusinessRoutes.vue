<template>
  <section class="rounded-lg border border-gray-100 p-3 dark:border-dark-700" data-testid="business-routes">
    <div class="flex flex-wrap items-center justify-between gap-2 text-sm">
      <h3 class="font-medium">{{ t('ticketBorrowing.routes.title') }}</h3>
      <span class="tabular-nums" data-testid="business-route-load">{{ t('ticketBorrowing.routes.load', { count: capacity.occupied, limit: capacity.limit ?? '—' }) }}</span>
    </div>
    <p class="mt-2 text-xs tabular-nums text-gray-500 dark:text-gray-400" data-testid="account-shared-capacity">{{ t('ticketBorrowing.routes.sharedCapacity', { business: capacity.business, background: capacity.background, remaining: capacity.remaining ?? '—' }) }}</p>
    <div class="mt-3 flex flex-wrap items-center gap-2 text-xs" data-testid="route-pool-summary">
      <span v-for="item in summary" :key="item.key" class="rounded-md px-2 py-1 tabular-nums" :class="item.key === 'accepting' && item.count > 0 ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'" :data-route-count="item.key">{{ t(`ticketBorrowing.statusSummary.${item.key}`, { count: item.count }) }}</span>
      <span class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.stored', counts) }}</span>
    </div>
    <p class="mt-2 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.routes.hint') }}</p>
    <div v-if="rows.length" class="mt-3 space-y-3">
      <article v-for="row in rows" :key="row.route.id" :data-business-route="row.route.id" class="rounded-lg border border-gray-200 dark:border-dark-600">
        <div class="grid grid-cols-2 gap-4 p-3 text-xs md:grid-cols-[minmax(0,1fr)_minmax(0,1.35fr)_minmax(0,1fr)_minmax(0,0.85fr)]">
          <div class="min-w-0">
            <div class="break-words font-mono text-sm font-medium" :title="row.route.gateway">{{ gateway(row.route.gateway) }}</div>
            <div class="mt-1 text-gray-500 dark:text-gray-400" :title="row.route.id">{{ row.route.id.slice(0, 8) }} · {{ t('ticketBorrowing.routes.source', { id: row.route.source_account_id }) }}</div>
            <BorrowTag v-if="row.route.id === target.preferred_route_id && row.state === 'accepting'" class="mt-2" kind="state" value="preferred" data-testid="preferred-route" />
          </div>
          <div class="min-w-0 space-y-1.5" data-testid="route-admission">
            <p class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.routes.state') }}</p>
            <BorrowTag kind="route" :value="row.state" />
            <p v-if="row.state === 'cooling'" class="text-amber-700 dark:text-amber-300">{{ seconds(row.route.cooldown_until) > 0 ? t('ticketBorrowing.statusSummary.recheckAfter', { time: duration(row.route.cooldown_until) }) : t('ticketBorrowing.routePool.checkDue') }}</p>
            <p v-else-if="row.state === 'busy'" class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.routes.accountBusy') }}</p>
            <p v-else-if="['pending_quality', 'pending_probe', 'checking'].includes(row.state)" class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.checkToResume') }}</p>
            <p v-if="row.route.checking && !['checking', 'expired', 'invalid'].includes(row.state)" class="text-blue-700 dark:text-blue-300">{{ t('ticketBorrowing.statusSummary.refreshingQuality') }}</p>
            <p v-if="row.state === 'cooling' && row.route.last_error"><BorrowTag kind="reason" :value="row.route.last_error" /></p>
          </div>
          <div class="min-w-0 space-y-1.5 tabular-nums">
            <p class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.businessRequests') }}</p>
            <p data-route-inflight class="font-medium">{{ t('ticketBorrowing.statusSummary.inflight', { count: row.route.inflight }) }}</p>
            <p v-if="row.state === 'accepting'" data-route-available class="text-emerald-700 dark:text-emerald-300">{{ t('ticketBorrowing.routes.sharedAdmission') }}</p>
            <p v-else data-route-available class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.noNewRequests') }}</p>
            <p v-if="!['accepting', 'busy'].includes(row.state) && row.route.inflight > 0" class="text-blue-700 dark:text-blue-300" data-testid="route-natural-drain">{{ t('ticketBorrowing.statusSummary.existingContinue') }}</p>
          </div>
          <div class="min-w-0 space-y-1.5 tabular-nums">
            <p class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.routeLifetime') }}</p>
            <p class="font-medium">{{ seconds(row.route.usable_until) > 0 ? duration(row.route.usable_until) : t('ticketBorrowing.routePool.expired') }}</p>
            <p v-if="seconds(row.route.usable_until) > 0" class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.deadlineAt', { time: date(row.route.usable_until) }) }}</p>
          </div>
        </div>
        <div class="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-gray-100 bg-gray-50/70 px-3 py-2 text-xs dark:border-dark-700 dark:bg-dark-800/40" data-testid="route-quality-result">
          <span class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.lastQuality') }}</span>
          <BorrowTag kind="quality" :value="row.quality" />
          <span v-if="row.route.quality?.answer">{{ t('ticketBorrowing.quality.answer') }}: {{ row.route.quality.answer }}</span>
          <span v-if="row.route.quality?.checked_at" class="text-gray-500 dark:text-gray-400">{{ date(row.route.quality.checked_at) }}</span>
          <span v-if="row.quality === 'passed'" class="text-emerald-700 dark:text-emerald-300">{{ t('ticketBorrowing.statusSummary.qualityRemaining', { time: duration(row.route.quality_valid_until) }) }}</span>
        </div>
        <div class="flex items-start justify-between gap-3 border-t border-gray-100 px-3 py-2 text-xs dark:border-dark-700">
          <details class="min-w-0 flex-1" data-testid="route-details">
            <summary class="w-fit cursor-pointer text-gray-500 hover:text-primary-600 dark:text-gray-400">{{ t('ticketBorrowing.statusSummary.details') }}</summary>
            <dl class="mt-2 grid gap-2 break-words leading-5 text-gray-600 dark:text-gray-300 sm:grid-cols-2">
              <div><dt class="text-gray-400">{{ t('ticketBorrowing.routes.route') }}</dt><dd class="break-all font-mono">{{ row.route.gateway }}<br />{{ row.route.id }}</dd></div>
              <div><dt class="text-gray-400">{{ t('ticketBorrowing.quality.title') }}</dt><dd>{{ row.route.quality?.model || '—' }} · {{ row.route.quality?.reasoning_effort || '—' }}<BorrowTag v-if="row.route.quality?.reason && row.quality !== 'passed'" class="ml-2" kind="reason" :value="row.route.quality.reason" /></dd></div>
              <div><dt class="text-gray-400">{{ t('ticketBorrowing.routes.published') }}</dt><dd>{{ date(row.route.published_at) }}</dd></div>
              <div><dt class="text-gray-400">{{ t('ticketBorrowing.routePool.lastSuccess') }}</dt><dd>{{ date(row.route.last_business_success_at) }}</dd></div>
              <div><dt class="text-gray-400">{{ t('ticketBorrowing.routePool.lastError') }}</dt><dd><BorrowTag kind="reason" :value="row.route.last_error" /> {{ date(row.route.last_error_at) }}</dd></div>
              <div><dt class="text-gray-400">{{ t('ticketBorrowing.routePool.model') }}</dt><dd>{{ row.route.verification_model || '—' }}</dd></div>
              <div v-if="row.route.deadline_source !== 'cookie'">{{ t('ticketBorrowing.routePool.conservativeDeadline') }}</div>
            </dl>
          </details>
          <button class="shrink-0 text-primary-600 hover:underline" @click="$emit('records', row.route.id)">{{ t('ticketBorrowing.management.relatedRecords') }}</button>
        </div>
      </article>
    </div>
    <p v-else class="mt-3 text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.routes.empty') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowTarget } from '@/api/admin/ticketBorrowing'
import { borrowAccountCapacity, borrowRouteCounts, borrowRouteQualityState, borrowRouteState, borrowSeconds } from '@/composables/borrowStatus'
import BorrowTag from './BorrowTag.vue'

const props = defineProps<{ target: BorrowTarget; now: number }>()
defineEmits<{ records: [routeId: string] }>()
const { t } = useI18n()
const seconds = (until?: string) => borrowSeconds(until, props.now)
const counts = computed(() => borrowRouteCounts(props.target, props.now))
const capacity = computed(() => borrowAccountCapacity(props.target))
const summary = computed(() => (['accepting', 'busy', 'pending', 'cooling', 'unavailable', 'expired', 'invalid'] as const)
  .map(key => ({ key, count: counts.value[key] })).filter(item => item.key === 'accepting' || item.count > 0))
const rows = computed(() => (props.target.business_routes ?? []).map(route => ({ route,
  state: borrowRouteState(route, props.now, props.target.enabled), quality: borrowRouteQualityState(route, props.now)
})))
function gateway(value: string) { return value.replace(/^chat\.gateway\.(unified-\d+)\.api\.openai\.com$/, '$1') }
function date(value?: string) { return value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleTimeString() : '—' }
function duration(value?: string) {
  const count = seconds(value)
  return count < 60 ? t('ticketBorrowing.seconds', { count }) : t('ticketBorrowing.statusSummary.minutesSeconds', { minutes: Math.floor(count / 60), seconds: count % 60 })
}
</script>
