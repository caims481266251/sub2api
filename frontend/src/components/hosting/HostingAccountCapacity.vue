<template>
  <div class="space-y-2 whitespace-nowrap" data-testid="hosting-capacity">
    <div class="flex items-center gap-2"><CapacityBadge :color-class="color(account.current_concurrency, account.settings.concurrency)" :current="account.current_concurrency ?? '—'" :max="account.settings.concurrency" :tooltip="t('hosting.currentCapacityHint')"><Icon name="server" size="xs" /></CapacityBadge></div>
    <div v-if="rpm > 0" class="flex items-center gap-2"><span class="text-xs text-gray-500">RPM</span><CapacityBadge :color-class="color(account.current_rpm, rpm)" :current="account.current_rpm ?? '—'" :max="rpm" :tooltip="t('hosting.currentRPMHint')"><Icon name="clock" size="xs" /></CapacityBadge></div>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import CapacityBadge from '@/components/account/CapacityBadge.vue'
import { Icon } from '@/components/icons'
import type { HostedAccount } from '@/api/hosting'
const props = defineProps<{ account: HostedAccount }>()
const { t } = useI18n()
const rpm = computed(() => Number(props.account.settings.extra.base_rpm) || 0)
function color(current: number | null | undefined, limit: number) {
  if (current != null && current >= limit) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (current != null && current > 0) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-400'
}
</script>
