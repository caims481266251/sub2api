<template>
  <div data-testid="target-probe-state" class="space-y-1">
    <BorrowTag kind="phase" :value="state.phase" />
    <p v-if="state.phase === 'draining'" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.management.phases.draining', { count: target.inflight ?? 0 }) }}</p>
    <p v-if="state.phase === 'probing'" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.management.probeInflight', { count: target.inflight ?? 0 }) }}</p>
    <div v-if="state.reason"><BorrowTag :value="state.reason" /></div>
    <BorrowReasonHint v-if="state.reason" :code="state.reason" />
    <template v-if="state.phase === 'budget' && state.reason === 'source_request_budget' && sourceBudget">
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.sourceBudgetUsed') }} {{ sourceBudget.used }} / {{ sourceBudget.limit }}</p>
      <p v-if="seconds(sourceBudget.available_at)" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.budgetWaitRemaining', { count: seconds(sourceBudget.available_at) }) }}</p>
    </template>
    <p v-else-if="state.until && seconds(state.until) > 0" class="text-xs text-gray-500 tabular-nums dark:text-gray-400">{{ t(state.phase === 'waiting' ? 'ticketBorrowing.replenishment.next' : 'ticketBorrowing.management.resumesIn', { seconds: seconds(state.until) }) }}</p>
    <div v-if="state.phase === 'waiting' && target.maintenance_state"><BorrowTag kind="maintenance" :value="target.maintenance_state" /></div>
  </div>
</template>

<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import BorrowReasonHint from './BorrowReasonHint.vue'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowSettings, BorrowTarget } from '@/api/admin/ticketBorrowing'
import { borrowProbeState, borrowSeconds } from '@/composables/borrowStatus'

const props = defineProps<{ target: BorrowTarget; running: boolean; now: number; sourceBudget?: BorrowSettings['source_budget'] }>()
const { t } = useI18n()
const state = computed(() => borrowProbeState(props.target, props.running, props.now))
const seconds = (until?: string) => borrowSeconds(until, props.now)
</script>
