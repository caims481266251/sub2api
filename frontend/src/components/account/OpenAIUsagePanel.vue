<template>
  <div class="space-y-1">
    <template v-if="usage?.five_hour || usage?.seven_day">
      <UsageProgressBar
        v-if="usage.five_hour"
        label="5h"
        :utilization="usage.five_hour.utilization"
        :resets-at="usage.five_hour.resets_at"
        :window-stats="usage.five_hour.window_stats"
        show-now-when-idle
        color="indigo"
      />
      <UsageProgressBar
        v-if="usage.seven_day"
        label="7d"
        :utilization="usage.seven_day.utilization"
        :resets-at="usage.seven_day.resets_at"
        :window-stats="usage.seven_day.window_stats"
        :estimated-total-cost="estimatedTotalCost"
        show-now-when-idle
        color="emerald"
      />
    </template>
    <div v-else-if="loading" class="space-y-1.5" role="status" :aria-label="t('common.loading')">
      <div v-for="window in 2" :key="window" class="flex items-center gap-1">
        <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
        <div class="h-1.5 w-8 animate-pulse rounded-full bg-gray-200 dark:bg-gray-700" />
        <div class="h-3 w-[32px] animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
      </div>
    </div>
    <div v-else class="text-xs text-gray-400">—</div>
    <p v-if="error" role="alert" class="max-w-xs text-xs text-red-500">{{ error }}</p>
    <OpenAIQuotaResetCell
      v-if="account.type === 'oauth'"
      :account="account"
      :transport="transport"
      :show-referral="showReferral"
      @account-updated="emit('account-updated', $event)"
    >
      <template #pre-actions>
        <button
          type="button"
          class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
          :disabled="activeQueryLoading || loading"
          @click="emit('refresh')"
        >
          <svg class="h-2.5 w-2.5" :class="{ 'animate-spin': activeQueryLoading }" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
          </svg>
          {{ t('admin.accounts.usageWindow.activeQuery') }}
        </button>
      </template>
    </OpenAIQuotaResetCell>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account, AccountUsageInfo } from '@/types'
import UsageProgressBar from './UsageProgressBar.vue'
import OpenAIQuotaResetCell from './OpenAIQuotaResetCell.vue'
import type { OpenAIQuotaAccount, OpenAIQuotaTransport } from './openAIQuotaTransport'

const props = withDefaults(defineProps<{
  account: OpenAIQuotaAccount
  usage: AccountUsageInfo | null
  loading?: boolean
  activeQueryLoading?: boolean
  error?: string | null
  transport?: OpenAIQuotaTransport
  showReferral?: boolean
}>(), { loading: false, activeQueryLoading: false, error: null, showReferral: true })
const emit = defineEmits<{ refresh: []; 'account-updated': [account: Account] }>()
const { t } = useI18n()
const estimatedTotalCost = computed(() => {
  const utilization = props.usage?.seven_day?.utilization
  const cost = props.usage?.seven_day?.window_stats?.cost
  if (typeof utilization !== 'number' || typeof cost !== 'number' || !Number.isFinite(utilization) || !Number.isFinite(cost) || utilization <= 0 || cost <= 0) return null
  const estimate = cost * 100 / utilization
  return Number.isFinite(estimate) && estimate > 0 ? estimate : null
})
</script>
