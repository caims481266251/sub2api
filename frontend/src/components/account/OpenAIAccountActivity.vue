<template>
  <div class="w-48 max-w-full space-y-1 border-t border-gray-100 pt-1.5 dark:border-dark-700" data-testid="openai-account-activity" @click.stop>
    <button type="button" class="flex w-full min-w-0 items-center gap-1.5 rounded text-left text-[11px] focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500"
      data-testid="open-quality-monitor" @click="emit('open-quality')">
      <span class="shrink-0 text-gray-500 dark:text-dark-300">{{ t('admin.accounts.activity.quality') }}</span>
      <span class="badge ml-auto shrink-0 text-[10px]" :class="qualityBadgeClass(activity?.quality.state)">{{ qualityLabel }}</span>
      <Icon name="chevronRight" size="xs" class="shrink-0 text-gray-400" />
    </button>
    <p v-if="checkedAt" class="text-right text-[10px] leading-4 text-gray-400 dark:text-dark-400"
      data-testid="quality-checked-at" :title="t('admin.accounts.activity.checkedAt') + ': ' + formatDateTime(checkedAt)">
      {{ checkedAgo }}
    </p>

    <div class="grid grid-cols-[repeat(16,minmax(0,1fr))] gap-[2px]" role="group" :aria-label="t('admin.accounts.activity.barLabel')">
      <span v-for="empty in 16 - records.length" :key="`empty-${empty}`" aria-hidden="true" data-testid="recent-request-empty"
        class="h-3 rounded-[2px] bg-gray-200 dark:bg-dark-600" />
      <HelpTooltip v-for="request in records" :key="request.attempt_id" class="!ml-0 !block min-w-0" width-class="w-72">
        <template #trigger>
          <button type="button" class="block h-3 w-full rounded-[2px] transition-opacity hover:opacity-80 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-500"
            :class="[requestColor(request), { 'request-failure': request.outcome === 'failure' }]"
            :aria-label="requestLabel(request)" :data-outcome="request.outcome" :data-tone="recentRequestTone(request)" data-testid="recent-request" />
        </template>
        <div class="space-y-1.5 text-left [overflow-wrap:anywhere]">
          <p class="font-semibold">{{ outcomeLabel(request) }}<span v-if="request.http_status > 0"> · HTTP {{ request.http_status }}</span></p>
          <p>{{ request.model || '—' }} · {{ t(request.source === 'quality' ? 'admin.accounts.activity.sourceQuality' : 'admin.accounts.activity.sourceBusiness') }}</p>
          <p>{{ t('admin.accounts.activity.firstToken') }}: {{ activityDuration(request.first_token_ms) }}<span v-if="request.first_token_ms == null"> · {{ t('admin.accounts.activity.unmeasured') }}</span></p>
          <p>{{ t('admin.accounts.activity.duration') }}: {{ activityDuration(request.duration_ms) }}</p>
          <p v-if="request.error_code || request.error_message" class="whitespace-pre-wrap text-red-200">{{ [request.error_code, request.error_message].filter(Boolean).join(' · ') }}</p>
          <p class="text-gray-300">{{ formatDateTime(request.completed_at) }}</p>
        </div>
      </HelpTooltip>
    </div>
    <p class="text-[10px] leading-4 text-gray-400 dark:text-dark-400" :class="{ 'text-amber-600 dark:text-amber-400': stale }">
      {{ stale ? t('admin.accounts.activity.stale') : activity?.recent_requests === null ? t('admin.accounts.activity.unavailable') : records.length ? t('admin.accounts.activity.recentCount', { count: records.length }) : loading ? t('common.loading') : t('admin.accounts.activity.noRequests') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useQualityDisplayClock } from '@/composables/useQualityDisplayClock'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'
import type { AccountActivity, AccountRecentRequest } from '@/api/admin/accountActivity'
import { activityDuration, qualityBadgeClass, recentRequestTone } from '@/utils/accountActivity'
import { LATENCY_BACKGROUND_CLASSES } from '@/utils/latencyHealth'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{ activity?: AccountActivity; stale?: boolean; loading?: boolean }>()
const emit = defineEmits<{ (event: 'open-quality'): void }>()
const { t } = useI18n()
const now = useQualityDisplayClock()
const checkedAt = computed(() => {
  const value = props.activity?.quality.latest_attempt?.checked_at
  return value && Number.isFinite(Date.parse(value)) ? value : null
})
const checkedAgo = computed(() => {
  if (!checkedAt.value) return ''
  const minutes = Math.floor(Math.max(0, now.value - Date.parse(checkedAt.value)) / 60000)
  const prefix = 'admin.accounts.activity.'
  const ago = minutes < 1 ? t(prefix + 'justChecked')
    : minutes < 60 ? t(prefix + 'checkedMinutesAgo', { count: minutes })
    : minutes < 1440 ? t(prefix + 'checkedHoursAgo', { count: Math.floor(minutes / 60) })
    : t(prefix + 'checkedDaysAgo', { count: Math.floor(minutes / 1440) })
  return ['running', 'queued', 'paused'].includes(props.activity?.quality.state ?? '')
    ? t(prefix + 'previousCheck', { time: ago }) : ago
})
const records = computed(() => (props.activity?.recent_requests ?? []).slice(-16))
const qualityLabel = computed(() => !props.activity && props.stale
  ? t('admin.accounts.activity.unavailable')
  : !props.activity && props.loading ? t('common.loading') : t(`admin.accounts.activity.states.${props.activity?.quality.state || 'untested'}`))

function requestColor(request: AccountRecentRequest) {
  const tone = recentRequestTone(request)
  return tone === 'unknown' ? 'bg-gray-400 dark:bg-dark-400' : LATENCY_BACKGROUND_CLASSES[tone]
}
function outcomeLabel(request: AccountRecentRequest) { return t(`admin.accounts.activity.outcomes.${request.outcome}`) }
function requestLabel(request: AccountRecentRequest) {
  return `${outcomeLabel(request)} · ${request.model} · ${t('admin.accounts.activity.firstToken')}: ${activityDuration(request.first_token_ms)} · ${formatDateTime(request.completed_at)}`
}
</script>

<style scoped>
.request-failure {
  background-image: repeating-linear-gradient(135deg, transparent 0, transparent 3px, rgb(255 255 255 / 45%) 3px, rgb(255 255 255 / 45%) 5px);
}
</style>
