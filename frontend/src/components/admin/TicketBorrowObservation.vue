<template>
  <details :open="expanded" class="mt-2" :data-candidate-id="details.candidate_id">
    <summary class="cursor-pointer text-xs text-primary-600 dark:text-primary-400">{{ t('ticketBorrowing.observationTitle') }}</summary>
    <div :class="expanded ? 'mt-3 space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600' : 'mt-2 max-h-96 min-w-[260px] max-w-lg space-y-3 overflow-y-auto rounded-lg bg-gray-50 p-3 dark:bg-dark-800'">
      <dl :class="expanded ? 'grid gap-4 text-xs sm:grid-cols-2' : 'space-y-2 text-xs'">
        <div v-for="row in rows" :key="row.key" class="min-w-0" :data-observation-field="row.key">
          <dt class="text-gray-500 dark:text-gray-400">{{ t(`ticketBorrowing.observationFields.${row.key}`) }}</dt>
          <dd class="mt-0.5 break-all font-mono">{{ row.value }}</dd>
        </div>
      </dl>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.observationHint') }}</p>
    </div>
  </details>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowCandidateDetails } from '@/api/admin/ticketBorrowing'

const props = defineProps<{ details: BorrowCandidateDetails; expanded?: boolean }>()
const { t } = useI18n()
const seconds = (value?: number) => value === undefined ? undefined : `${(value / 1000).toFixed(3)} s`
const rows = computed(() => {
  const d = props.details
  const entries: [string, string | number | undefined][] = [
    ['replenishmentUntil', d.replenishment?.until], ['replenishmentMode', d.replenishment?.shadow === undefined ? undefined : t(d.replenishment.shadow ? 'ticketBorrowing.replenishment.shadow' : 'ticketBorrowing.replenishment.publish')],
    ['replenishmentInventory', d.replenishment?.inventory], ['replenishmentExit', d.replenishment?.exit_reason],
    ['sourceEnvironmentVersion', d.source_environment_version ?? d.replenishment?.environment_version],
    ['rotationJob', d.rotation?.job_id], ['rotationRole', d.rotation?.role], ['rotationTrigger', d.rotation?.trigger], ['rotationProxy', d.rotation ? d.rotation.previous_entry_id === d.rotation.entry_id ? `#${d.rotation.entry_id}` : `${d.rotation.previous_entry_id || '—'} → ${d.rotation.entry_id}` : undefined], ['rotationVersion', d.rotation?.environment_version], ['rotationChoice', d.rotation?.selection_kind ? t(`ticketBorrowing.rotation.choices.${d.rotation.selection_kind}`) : undefined],
    ['gateway', d.gateway], ['sourceModel', d.source_model],
    ['cookieAgeAtProbe', seconds(d.cookie_age_at_probe_ms)], ['remainingAtProbe', seconds(d.remaining_at_probe_ms)],
    ['cookieAgeAtRecord', seconds(d.cookie_age_ms)], ['remainingAtRecord', seconds(d.remaining_ms)],
    ['sourceAttempts', d.source_attempts], ['probeRetries', d.probe_verdict ? (d.probe_retries ?? 0) : undefined],
    ['httpStatus', d.mint_status || d.continue_status ? `${d.mint_status || '—'} / ${d.continue_status || '—'}` : undefined],
    ['sourceVersion', d.source?.client_version], ['targetVersion', d.target?.client_version],
    ['sourceFormat', d.source_format], ['candidateID', d.candidate_id], ['batchID', d.batch_id],
    ['cookieHash', d.cookie_hash], ['sourceIdentity', d.source?.identity_hash], ['targetIdentity', d.target?.identity_hash],
    ['sourceProfile', d.source?.profile_hash], ['targetProfile', d.target?.profile_hash],
    ['sourceProxy', d.source?.configured_proxy_hash], ['targetProxy', d.target?.configured_proxy_hash],
    ['sourceBody', d.source?.body_hash], ['targetBody', d.target?.body_hash]
  ]
  return entries.filter(([, value]) => value !== undefined && value !== '').map(([key, value]) => ({ key, value }))
})
</script>
