<template>
  <section v-if="rows.length" class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
    <h4 class="font-semibold">{{ t('requestTiming.borrowAttempts') }}</h4>
    <div v-for="({ attempt: a, recovery }, index) in rows" :key="a.number" class="rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-800">
      <div class="flex flex-wrap gap-2 font-medium">
        <span>#{{ index + 1 }} · {{ t('requestTiming.account') }} #{{ a.account_id }}</span>
        <span class="rounded bg-gray-200 px-2 dark:bg-dark-600">HTTP {{ a.status || '—' }}</span>
        <span v-if="a.no_text_long_tail" class="rounded bg-amber-100 px-2 text-amber-800 dark:bg-amber-950 dark:text-amber-200">{{ t('requestTiming.noTextLongTail') }}</span>
      </div>
      <p class="mt-2 break-all font-mono text-xs">{{ t('requestTiming.borrowRoute') }}: {{ a.borrow_route_id }} · {{ a.model || '—' }} / {{ a.reasoning_effort || '—' }}</p>
      <dl class="mt-2 grid gap-2 sm:grid-cols-2">
        <div>{{ t('requestTiming.requestWritten') }}: {{ a.events.request_written != null ? t('requestTiming.yes') : t('requestTiming.missing') }}</div>
        <div>{{ t('requestTiming.retryAfter') }}: {{ seconds(a.retry_after_ms) }}</div>
        <div>{{ t('requestTiming.firstReasoning') }}: {{ offset(a, 'first_reasoning') }} · {{ a.reasoning_events ?? 0 }}</div>
        <div>{{ t('requestTiming.firstText') }}: {{ offset(a, 'first_text') }}</div>
        <div v-if="recovery">{{ t('requestTiming.recoverySends') }}: {{ recovery.requests_sent }} / 2</div>
        <div v-if="recovery?.stop_reason">{{ t('requestTiming.recoveryStop') }}: {{ te(`requestTiming.recoveryReasons.${recovery.stop_reason}`) ? t(`requestTiming.recoveryReasons.${recovery.stop_reason}`) : recovery.stop_reason }}</div>
      </dl>
    </div>
    <p class="text-xs text-gray-500">{{ t('requestTiming.borrowObservationNote') }}</p>
  </section>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { BorrowAttemptDetail } from '@/utils/borrowAttemptDetails'
import type { TimingAttempt } from '@/api/admin/usageTiming'
defineProps<{ rows: BorrowAttemptDetail[] }>()
const { t, te } = useI18n()
const seconds = (ms: number | undefined) => ms != null && Number.isFinite(ms) ? `${(ms / 1000).toFixed(2)} s` : '—'
const offset = (a: TimingAttempt, event: string) => a.events[event] == null ? '—' : seconds(Math.max(0, a.events[event] - a.start_ms))
</script>
