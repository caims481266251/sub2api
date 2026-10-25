<template>
  <details :open="expanded" class="mt-2" :data-candidate-id="details.candidate_id">
    <summary class="cursor-pointer text-xs text-primary-600 dark:text-primary-400">{{ t('ticketBorrowing.observationTitle') }}</summary>
    <div :class="expanded ? 'mt-3 space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600' : 'mt-2 max-h-96 min-w-[260px] max-w-lg space-y-3 overflow-y-auto rounded-lg bg-gray-50 p-3 dark:bg-dark-800'">
      <dl :class="expanded ? 'grid gap-4 text-xs sm:grid-cols-2' : 'space-y-2 text-xs'">
        <div v-for="row in rows" :key="row.key" class="min-w-0" :data-observation-field="row.key">
          <dt class="text-gray-500 dark:text-gray-400">{{ t(`ticketBorrowing.observationFields.${row.key}`) }}</dt>
          <dd class="mt-0.5"><BorrowTag v-if="row.kind" :kind="row.kind" :value="row.value" detail /><span v-else class="break-all font-mono">{{ row.value }}</span></dd>
        </div>
        <div v-if="details.mint_status !== undefined || details.continue_status !== undefined" class="min-w-0 sm:col-span-2" data-observation-field="httpStatus">
          <dt class="text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.observationFields.httpStatus') }}</dt>
          <dd class="mt-1 flex flex-wrap gap-x-4 gap-y-2">
            <span class="inline-flex flex-wrap items-center gap-2">{{ t('ticketBorrowing.observationFields.mintHTTP') }}<BorrowTag kind="http" :value="details.mint_status" /></span>
            <span class="inline-flex flex-wrap items-center gap-2">{{ t('ticketBorrowing.observationFields.continueHTTP') }}<BorrowTag kind="http" :value="details.continue_status" /></span>
          </dd>
          <dd class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.tags.httpHint') }}</dd>
        </div>
      </dl>
      <div v-for="phase in phases" :key="phase.name" class="space-y-2 border-t border-gray-200 pt-3 text-xs dark:border-dark-600" :data-probe-phase="phase.name">
        <p class="font-medium">{{ diagnosticLabel('phases', phase.name) }} · {{ seconds(phase.duration) }} · {{ t('ticketBorrowing.diagnostics.retryCount', { count: phase.retries }) }}</p>
        <div v-for="attempt in phase.attempts" :key="attempt.number" class="space-y-1 rounded bg-gray-50 p-2 dark:bg-dark-800">
          <p>#{{ attempt.number }} · {{ seconds(attempt.duration_ms) }} <span v-if="attempt.status">· HTTP {{ attempt.status }}</span></p>
          <p v-if="attempt.error_class">{{ t('ticketBorrowing.diagnostics.errorClass') }}: {{ diagnosticLabel('errors', attempt.error_class) }}</p>
          <p v-if="attempt.timeout_stage || attempt.failure_stage">{{ t(attempt.timeout_stage ? 'ticketBorrowing.diagnostics.timeoutStage' : 'ticketBorrowing.diagnostics.failureStage') }}: {{ diagnosticLabel('stages', attempt.timeout_stage || attempt.failure_stage || '') }}</p>
          <p v-if="attempt.cancellation_cause">{{ t('ticketBorrowing.diagnostics.cancellation') }}: {{ diagnosticLabel('causes', attempt.cancellation_cause) }}</p>
          <dl class="grid grid-cols-2 gap-x-3 gap-y-1"><template v-for="[event, ms] in timeline(attempt.events)" :key="event"><dt class="text-gray-500 dark:text-gray-400">{{ diagnosticLabel('events', String(event)) }}</dt><dd class="font-mono">+{{ seconds(ms) }}</dd></template></dl>
        </div>
      </div>
      <p v-if="phases.length" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.diagnostics.timelineHint') }}</p>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.observationHint') }}</p>
    </div>
  </details>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BorrowCandidateDetails } from '@/api/admin/ticketBorrowing'
import type { BorrowTagKind } from '@/composables/borrowTags'
import BorrowTag from './BorrowTag.vue'

const props = defineProps<{ details: BorrowCandidateDetails; expanded?: boolean }>()
const { t, te } = useI18n()
const diagnosticLabel = (group: string, value: string) => {
  const key = `ticketBorrowing.diagnostics.${group}.${value}`
  return te(key) ? t(key) : value
}
const eventOrder = ['slot_acquired', 'admitted', 'connection_start', 'dns_start', 'dns_end', 'tcp_start', 'tcp_end', 'tls_start', 'tls_end', 'connection_ready', 'headers_written', 'request_written', 'first_byte', 'response_headers', 'first_sse', 'completed', 'failed', 'eof']
const timeline = (events: Record<string, number>) => Object.entries(events).sort(([a, left], [b, right]) => left - right || eventOrder.indexOf(a) - eventOrder.indexOf(b))
const phases = computed(() => {
  const d = props.details
  return [
    ...(d.source_http ? [{ name: 'source', duration: d.source_http.duration_ms, retries: 0, attempts: [d.source_http] }] : []),
    ...(['mint', 'continue'] as const).flatMap(name => d[name] ? [{ name, duration: d[name].duration_ms, retries: d[name].retry_count, attempts: d[name].attempts }] : [])
  ]
})
const seconds = (value?: number) => value === undefined ? undefined : `${(value / 1000).toFixed(3)} s`
const rows = computed(() => {
  const d = props.details
  const entries: [string, string | number | undefined, BorrowTagKind?][] = [
    ['acquisitionIdentity', d.acquisition_identity ? t(`ticketBorrowing.simplified.${d.acquisition_identity}`) : undefined],
    ['acquisitionStarted', d.acquisition_started_at], ['admissionUntil', d.admission_until], ['upstreamExpires', d.upstream_expires_at],
    ['poolEntry', d.pool_entry_id], ['allocationRound', d.allocation_round],
    ['probeVerdict', d.probe_verdict, 'verdict'], ['probeFailure', d.probe_failure, 'failure'],
    ['replenishmentUntil', d.replenishment?.until], ['replenishmentMode', d.replenishment?.shadow === undefined ? undefined : d.replenishment.shadow ? 'verificationOnly' : 'publicationEnabled', 'state'],
    ['replenishmentInventory', d.replenishment?.inventory], ['replenishmentExit', d.replenishment?.exit_reason, 'reason'],
    ['sourceEnvironmentVersion', d.source_environment_version ?? d.replenishment?.environment_version],
    ['rotationJob', d.rotation?.job_id], ['rotationRole', d.rotation?.role === 'source' ? 'sourceRole' : d.rotation?.role === 'target' ? 'targetRole' : d.rotation?.role, 'state'], ['rotationTrigger', d.rotation?.trigger, 'trigger'], ['rotationProxy', d.rotation ? d.rotation.previous_entry_id === d.rotation.entry_id ? `#${d.rotation.entry_id}` : `${d.rotation.previous_entry_id || '—'} → ${d.rotation.entry_id}` : undefined], ['rotationVersion', d.rotation?.environment_version], ['rotationChoice', d.rotation?.selection_kind ? t(`ticketBorrowing.rotation.choices.${d.rotation.selection_kind}`) : undefined],
    ['gatewayPriority', d.gateway_priority === 'normal' ? 'gatewayNormal' : d.gateway_priority === 'prioritized' ? 'gatewayPrioritized' : d.gateway_priority === 'recheck' ? 'gatewayRecheck' : d.gateway_priority, 'state'],
    ['poolEnqueuedAt', d.pool_enqueued_at], ['poolClaimedAt', d.pool_claimed_at], ['poolMinimumRemaining', seconds(d.pool_minimum_remaining_ms)],
    ['probeQueue', seconds(d.probe_queue_ms)], ['cancellationCause', d.cancellation_cause ? diagnosticLabel('causes', d.cancellation_cause) : undefined], ['sourceQueue', seconds(d.source_queue_ms)], ['sourcePreparation', seconds(d.source_preparation_ms)], ['sourceNetwork', seconds(d.source_network_ms)], ['publication', seconds(d.publication_ms)],
    ['gateway', d.gateway], ['sourceModel', d.source_model],
    ['cookieAgeAtProbe', seconds(d.cookie_age_at_probe_ms)], ['remainingAtProbe', seconds(d.remaining_at_probe_ms)],
    ['cookieAgeAtRecord', seconds(d.cookie_age_ms)], ['remainingAtRecord', seconds(d.remaining_ms)],
    ['sourceAttempts', d.source_attempts], ['probeRetries', d.probe_verdict ? (d.probe_retries ?? 0) : undefined],
    ['sourceVersion', d.source?.client_version], ['targetVersion', d.target?.client_version],
    ['sourceFormat', d.source_format], ['candidateID', d.candidate_id], ['batchID', d.batch_id],
    ['cookieHash', d.cookie_hash], ['sourceIdentity', d.source?.identity_hash], ['targetIdentity', d.target?.identity_hash],
    ['sourceProfile', d.source?.profile_hash], ['targetProfile', d.target?.profile_hash],
    ['sourceProxy', d.source?.configured_proxy_hash], ['targetProxy', d.target?.configured_proxy_hash],
    ['businessIdentity', d.business?.identity_hash], ['businessProfile', d.business?.profile_hash], ['businessProxy', d.business?.configured_proxy_hash],
    ['sourceBody', d.source?.body_hash], ['targetBody', d.target?.body_hash]
  ]
  return entries.filter(([, value]) => value !== undefined && value !== '').map(([key, value, kind]) => ({ key, value, kind }))
})
</script>
