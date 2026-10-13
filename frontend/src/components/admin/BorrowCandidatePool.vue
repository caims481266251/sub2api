<template>
  <section class="space-y-3" data-testid="candidate-pool">
    <div class="card space-y-3 p-4">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex flex-wrap items-center gap-3">
          <h2 class="font-medium">{{ t('ticketBorrowing.candidatePool.title') }}</h2>
          <BorrowTag v-if="snapshot" kind="state" :value="snapshot.summary.running ? 'running' : 'paused'" />
          <span class="text-xs text-gray-500" role="status">{{ error ? t('ticketBorrowing.management.staleStatus') : lastUpdated ? t('ticketBorrowing.management.updatedAgo', { seconds: Math.max(0, Math.floor((now - lastUpdated) / 1000)) }) : t('ticketBorrowing.statusUnknown') }}</span>
        </div>
        <div class="flex items-center gap-3">
          <label class="flex items-center gap-2 text-xs"><input v-model="autoRefresh" type="checkbox" />{{ t('ticketBorrowing.candidatePool.autoRefresh') }}</label>
          <button class="btn btn-secondary btn-sm" :disabled="loading || saving" data-testid="pool-refresh" @click="refresh">{{ t(loading ? 'ticketBorrowing.loading' : 'ticketBorrowing.refresh') }}</button>
        </div>
      </div>
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-5">
        <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-900">
          <div class="text-xs text-gray-500">{{ t('ticketBorrowing.candidatePool.occupancy') }}</div>
          <div class="mt-1 text-xl font-semibold tabular-nums" data-testid="pool-occupancy">{{ snapshot ? `${snapshot.summary.occupied} / ${snapshot.summary.capacity}` : '—' }}</div>
        </div>
        <button v-for="key in activeStates" :key="key" class="rounded-lg bg-gray-50 p-3 text-left hover:bg-gray-100 dark:bg-dark-900 dark:hover:bg-dark-700" @click="state = key">
          <BorrowTag kind="pool" :value="key" /><div class="mt-1 text-xl font-semibold tabular-nums">{{ snapshot?.summary[key] ?? '—' }}</div>
        </button>
        <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-900"><div class="text-xs text-gray-500">{{ t('ticketBorrowing.candidatePool.waitingTargets') }}</div><div class="mt-1 text-xl font-semibold tabular-nums">{{ snapshot?.summary.waiting_targets ?? '—' }}</div><p v-if="snapshot" class="mt-1 text-xs text-gray-500">{{ t('ticketBorrowing.candidatePool.demandSlots', { count: snapshot.summary.demand_slots }) }}</p></div>
      </div>
      <p v-if="snapshot?.summary.production_state" class="text-xs text-gray-600 dark:text-gray-300" data-testid="pool-production">{{ t(`ticketBorrowing.candidatePool.production.${snapshot.summary.production_state}`) }}</p>
      <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.candidatePool.hint') }}</p>
      <p v-if="snapshot && snapshot.summary.occupied > snapshot.summary.capacity" class="rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300" data-testid="pool-over-capacity">{{ t('ticketBorrowing.candidatePool.draining') }}</p>
      <form class="flex flex-wrap items-end gap-3 border-t border-gray-100 pt-3 dark:border-dark-700" @submit.prevent="saveCapacity">
        <label class="space-y-1 text-sm"><span class="block">{{ t('ticketBorrowing.candidatePool.capacity') }}</span><input v-model="capacity" type="number" min="1" max="1024" step="1" class="input w-32" :disabled="!snapshot || saving" data-testid="pool-capacity" /></label>
        <button class="btn btn-primary btn-sm" :disabled="!dirty || !validCapacity || conflict || saving || !snapshot" data-testid="pool-capacity-save">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
        <button v-if="dirty || conflict" type="button" class="btn btn-secondary btn-sm" :disabled="saving" @click="resetCapacity">{{ t('ticketBorrowing.candidatePool.useSaved') }}</button>
        <span class="text-xs text-gray-500">{{ t('ticketBorrowing.candidatePool.capacityHint') }}</span>
        <p v-if="dirty && !validCapacity" role="alert" class="w-full text-sm text-red-600">{{ t('ticketBorrowing.candidatePool.capacityInvalid') }}</p>
        <p v-if="conflict" role="alert" class="w-full text-sm text-amber-700">{{ t('ticketBorrowing.candidatePool.conflict') }}</p>
        <p v-if="saveError" role="alert" class="w-full text-sm text-red-600">{{ saveError }}</p>
        <p v-if="saved && !dirty" role="status" class="w-full text-sm text-emerald-600">{{ t('ticketBorrowing.candidatePool.saved') }}</p>
      </form>
    </div>

    <div class="card space-y-3 p-4">
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Select v-model="state" :options="stateOptions" :aria-label="t('ticketBorrowing.candidatePool.state')" data-testid="pool-state" />
        <Select v-model="sourceAccount" :options="sourceOptions" searchable :aria-label="t('ticketBorrowing.candidatePool.source')" data-testid="pool-source" />
        <Select v-model="targetAccount" :options="targetOptions" searchable :aria-label="t('ticketBorrowing.candidatePool.target')" data-testid="pool-target" />
        <input v-model="searchDraft" class="input" maxlength="200" :placeholder="t('ticketBorrowing.candidatePool.search')" :aria-label="t('ticketBorrowing.candidatePool.search')" data-testid="pool-search" />
      </div>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ t('ticketBorrowing.candidatePool.loadFailed') }}</p>
      <p v-if="page > 1 || state === 'finished'" class="text-xs text-gray-500">{{ t('ticketBorrowing.candidatePool.historyRefresh') }}</p>
      <div class="overflow-x-auto" :aria-busy="loading">
        <table class="pool-table w-full text-left text-xs">
          <thead class="text-gray-500 dark:text-gray-400"><tr><th>{{ t('ticketBorrowing.candidatePool.candidate') }}</th><th>{{ t('ticketBorrowing.candidatePool.state') }}</th><th>{{ t('ticketBorrowing.candidatePool.sourceTarget') }}</th><th>{{ t('ticketBorrowing.candidatePool.timing') }}</th><th>{{ t('ticketBorrowing.candidatePool.remaining') }}</th><th>{{ t('ticketBorrowing.candidatePool.result') }}</th><th>{{ t('common.actions') }}</th></tr></thead>
          <tbody>
            <tr v-for="item in snapshot?.items ?? []" :key="item.id" :data-pool-candidate="item.id" class="border-t border-gray-100 dark:border-dark-700">
              <td><button class="break-all text-left font-mono text-primary-600" :title="item.id" @click="openDetail(item.id)">{{ item.id.slice(0, 12) }}</button><div class="mt-1 break-all text-gray-500">{{ item.gateway || '—' }}</div></td>
              <td :data-label="t('ticketBorrowing.candidatePool.state')"><BorrowTag kind="pool" :value="item.state" /><div v-if="item.gateway_priority" class="mt-1"><BorrowTag kind="state" :value="item.gateway_priority === 'recheck' ? 'gatewayRecheck' : item.gateway_priority === 'prioritized' ? 'gatewayPrioritized' : 'gatewayNormal'" /></div></td>
              <td :data-label="t('ticketBorrowing.candidatePool.sourceTarget')"><div>{{ item.source_name || '—' }} <span class="text-gray-400">#{{ item.source_account_id || item.source_id }}</span></div><div class="mt-1 text-gray-500">→ {{ item.target_id ? `${item.target_name} #${item.target_id}` : t('ticketBorrowing.candidatePool.unclaimed') }}</div></td>
              <td :data-label="t('ticketBorrowing.candidatePool.timing')" class="tabular-nums"><div>{{ date(item.created_at) }}</div><div class="mt-1 text-gray-500">{{ t('ticketBorrowing.candidatePool.stageElapsed', { seconds: elapsed(item) }) }}</div><div v-if="item.enqueued_at" class="mt-1 text-gray-500">{{ t('ticketBorrowing.candidatePool.queueAge', { seconds: queueAge(item) }) }}</div></td>
              <td :data-label="t('ticketBorrowing.candidatePool.remaining')" class="tabular-nums"><template v-if="item.state !== 'finished'"><div>{{ item.cookie_expires_at ? t('ticketBorrowing.candidatePool.cookieRemaining', { seconds: remaining(item.cookie_expires_at) }) : t('ticketBorrowing.candidatePool.expiryUnknown') }}</div><div v-if="item.admission_until" class="mt-1 text-gray-500">{{ t('ticketBorrowing.candidatePool.retainedRemaining', { seconds: remaining(item.admission_until) }) }}</div></template><span v-else>—</span></td>
              <td :data-label="t('ticketBorrowing.candidatePool.result')"><BorrowTag v-if="item.route_id" kind="state" value="published" /><BorrowTag v-else :value="item.reason" /></td>
              <td><div class="flex flex-wrap gap-2"><button class="text-primary-600" :data-pool-detail="item.id" @click="openDetail(item.id)">{{ t('ticketBorrowing.candidatePool.details') }}</button><button class="text-primary-600" @click="records(item)">{{ t('ticketBorrowing.management.relatedRecords') }}</button><button v-if="item.route_id && item.target_id" class="text-primary-600" @click="openTarget(item.target_id)">{{ t('ticketBorrowing.candidatePool.targetRoutes') }}</button></div></td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-if="snapshot && !snapshot.items.length" class="py-6 text-center text-sm text-gray-500">{{ t('ticketBorrowing.candidatePool.empty') }}</p>
      <BorrowListPagination v-if="snapshot" v-model:page="page" v-model:page-size="pageSize" :total="snapshot.total" />
    </div>

    <BaseDialog :show="!!detailID" :title="t('ticketBorrowing.candidatePool.details')" width="wide" placement="right" @close="closeDetail">
      <p v-if="detailLoading" role="status">{{ t('ticketBorrowing.loading') }}</p>
      <p v-if="detailError" role="alert" class="text-sm text-red-600">{{ detailError }} <button class="underline" @click="loadDetail">{{ t('ticketBorrowing.refresh') }}</button></p>
      <template v-if="detail">
        <div class="flex flex-wrap items-center gap-2"><BorrowTag kind="pool" :value="detail.state" /><BorrowTag :value="detail.reason" /><BorrowTag v-if="detail.route_id" kind="state" value="published" /></div>
        <p class="mt-3 break-all font-mono text-xs">{{ detail.id }}</p>
        <dl class="mt-4 grid gap-3 text-sm sm:grid-cols-2"><div v-for="row in detailTimes" :key="row.key"><dt class="text-xs text-gray-500">{{ t(`ticketBorrowing.candidatePool.${row.key}`) }}</dt><dd>{{ date(row.value) }}</dd></div></dl>
        <div class="my-4 flex flex-wrap gap-3"><button class="btn btn-secondary btn-sm" @click="records(detail)">{{ t('ticketBorrowing.management.relatedRecords') }}</button><button v-if="detail.route_id && detail.target_id" class="btn btn-secondary btn-sm" @click="openTarget(detail.target_id)">{{ t('ticketBorrowing.candidatePool.targetRoutes') }}</button></div>
        <TicketBorrowObservation v-if="detail.details" :details="detail.details" expanded />
      </template>
    </BaseDialog>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ticketBorrowing as api, type BorrowCandidatePoolPage, type BorrowCandidatePoolQuery, type BorrowCandidatePoolItem, type BorrowCandidatePoolDetail, type BorrowCandidatePoolConfig, type BorrowSource, type BorrowTarget, type BorrowEventQuery } from '@/api/admin/ticketBorrowing'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import BorrowTag from './BorrowTag.vue'
import BorrowListPagination from './BorrowListPagination.vue'
import TicketBorrowObservation from './TicketBorrowObservation.vue'

const props = defineProps<{ visible: boolean; sources: BorrowSource[]; targets: BorrowTarget[] }>()
const emit = defineEmits<{ records: [query: BorrowEventQuery]; target: [id: number]; dirty: [value: boolean]; saving: [value: boolean] }>()
const { t } = useI18n()
const snapshot = ref<BorrowCandidatePoolPage>()
const loading = ref(false), error = ref(false), saving = ref(false), saveError = ref(''), saved = ref(false)
const capacity = ref<number | string>(64), baseline = ref<BorrowCandidatePoolConfig>()
const dirty = computed(() => !!baseline.value && Number(capacity.value) !== baseline.value.capacity)
const validCapacity = computed(() => Number.isInteger(Number(capacity.value)) && Number(capacity.value) >= 1 && Number(capacity.value) <= 1024)
const conflict = computed(() => !!baseline.value && !!snapshot.value && baseline.value.revision !== snapshot.value.summary.revision)
watch(dirty, value => emit('dirty', value))
watch(saving, value => emit('saving', value))
const activeStates = ['acquiring', 'queued', 'claimed'] as const
const state = ref<NonNullable<BorrowCandidatePoolQuery['state']>>('active')
const sourceAccount = ref(0), targetAccount = ref(0), searchDraft = ref(''), search = ref(''), page = ref(1), pageSize = ref(20)
const autoRefresh = ref(true), lastUpdated = ref(0), now = ref(Date.now()), offset = ref(0)
const stateOptions = computed(() => ['active', ...activeStates, 'finished', 'all'].map(value => ({ value, label: t(`ticketBorrowing.candidatePool.states.${value}`) })))
const sourceOptions = computed(() => [{ value: 0, label: t('ticketBorrowing.candidatePool.allSources') }, ...props.sources.map(s => ({ value: s.account_id, label: `${s.name} #${s.account_id}` }))])
const targetOptions = computed(() => [{ value: 0, label: t('ticketBorrowing.candidatePool.allTargets') }, ...props.targets.map(a => ({ value: a.account_id, label: `${a.name} #${a.account_id}` }))])
let disposed = false, generation = 0, debounce: ReturnType<typeof setTimeout> | undefined
function resetCapacity() {
  if (!snapshot.value) return
  baseline.value = { capacity: snapshot.value.summary.capacity, revision: snapshot.value.summary.revision }
  capacity.value = baseline.value.capacity; saveError.value = ''; saved.value = false
}
async function refresh() {
  if (disposed || !props.visible || saving.value) return
  const current = ++generation
  loading.value = true
  try {
    const value = await api.candidatePool({ state: state.value, source_account_id: sourceAccount.value || undefined, target_id: targetAccount.value || undefined, search: search.value || undefined, page: page.value, page_size: pageSize.value })
    if (disposed || current !== generation) return
    const keepDraft = dirty.value
    snapshot.value = value; offset.value = Date.parse(value.server_now) - Date.now(); lastUpdated.value = Date.now(); error.value = false
    if (!keepDraft) {
      baseline.value = { capacity: value.summary.capacity, revision: value.summary.revision }
      capacity.value = value.summary.capacity
    }
  } catch { if (!disposed && current === generation) error.value = true }
  finally { if (current === generation) loading.value = false }
}
async function saveCapacity() {
  if (!baseline.value || !dirty.value || !validCapacity.value || conflict.value || saving.value) return
  saving.value = true; saveError.value = ''; saved.value = false; generation++
  try {
    const value = await api.saveCandidatePoolConfig({ capacity: Number(capacity.value), revision: baseline.value.revision })
    if (disposed) return
    baseline.value = value; capacity.value = value.capacity
    if (snapshot.value) snapshot.value.summary = { ...snapshot.value.summary, ...value }
    saved.value = true
  } catch (e) {
    if (!disposed) saveError.value = t((e as { response?: { status?: number } }).response?.status === 409 ? 'ticketBorrowing.candidatePool.conflict' : 'ticketBorrowing.candidatePool.saveFailed')
  } finally {
    saving.value = false; loading.value = false
    if (!disposed) await refresh()
  }
}
watch([state, sourceAccount, targetAccount, search, pageSize], () => { if (page.value !== 1) page.value = 1; else void refresh() })
watch(page, () => { void refresh() })
watch(searchDraft, value => { clearTimeout(debounce); debounce = setTimeout(() => { search.value = value.trim() }, 300) })
watch(() => props.visible, value => { if (value) void refresh(); else { generation++; loading.value = false; closeDetail() } })
function date(value?: string) { return value ? new Date(value).toLocaleString() : '—' }
function remaining(value: string) { return Math.max(0, Math.ceil((Date.parse(value) - now.value - offset.value) / 1000)) }
function elapsed(item: BorrowCandidatePoolItem) {
  const start = item.claimed_at || item.enqueued_at || item.created_at
  return Math.max(0, ((item.finished_at ? Date.parse(item.finished_at) : now.value + offset.value) - Date.parse(start)) / 1000).toFixed(1)
}
function queueAge(item: BorrowCandidatePoolItem) {
  if (!item.enqueued_at) return '0.0'
  const end = item.claimed_at || item.finished_at
  return Math.max(0, ((end ? Date.parse(end) : now.value + offset.value) - Date.parse(item.enqueued_at)) / 1000).toFixed(1)
}
const detailID = ref(''), detail = ref<BorrowCandidatePoolDetail>(), detailLoading = ref(false), detailError = ref('')
let detailGeneration = 0
const detailTimes = computed(() => [
  { key: 'createdAt', value: detail.value?.created_at }, { key: 'enqueuedAt', value: detail.value?.enqueued_at },
  { key: 'claimedAt', value: detail.value?.claimed_at }, { key: 'finishedAt', value: detail.value?.finished_at },
  { key: 'admissionUntil', value: detail.value?.admission_until }, { key: 'cookieExpiresAt', value: detail.value?.cookie_expires_at }
])
function closeDetail() { detailGeneration++; detailID.value = ''; detail.value = undefined; detailLoading.value = false; detailError.value = '' }
function openDetail(id: string) { closeDetail(); detailID.value = id; void loadDetail() }
async function loadDetail() {
  if (!detailID.value || disposed) return
  const current = ++detailGeneration
  detailLoading.value = true
  try {
    const value = await api.candidatePoolDetail(detailID.value)
    if (!disposed && current === detailGeneration) { detail.value = value; detailError.value = '' }
  } catch (e) {
    if (!disposed && current === detailGeneration) detailError.value = t((e as { response?: { status?: number } }).response?.status === 404 ? 'ticketBorrowing.candidatePool.notFound' : 'ticketBorrowing.candidatePool.loadFailed')
  } finally { if (current === detailGeneration) detailLoading.value = false }
}
function records(item: BorrowCandidatePoolItem) { closeDetail(); emit('records', { candidate_id: item.id }) }
function openTarget(id: number) { closeDetail(); emit('target', id) }
function visibilityChanged() { if (document.visibilityState === 'visible' && props.visible && autoRefresh.value && page.value === 1 && state.value !== 'finished') void refresh() }
const clock = setInterval(() => { now.value = Date.now() }, 1000)
const poll = setInterval(() => {
  if (props.visible && document.visibilityState !== 'hidden' && autoRefresh.value && !loading.value && !saving.value && page.value === 1 && state.value !== 'finished') { void refresh(); if (detailID.value && !detailLoading.value) void loadDetail() }
}, 5000)
onMounted(() => { document.addEventListener('visibilitychange', visibilityChanged); if (props.visible) void refresh() })
onBeforeUnmount(() => { disposed = true; generation++; detailGeneration++; clearInterval(clock); clearInterval(poll); clearTimeout(debounce); document.removeEventListener('visibilitychange', visibilityChanged) })
defineExpose({ refresh })
</script>

<style scoped>
.pool-table th, .pool-table td { @apply px-2 py-3 align-top; }
.pool-table th { @apply whitespace-nowrap font-medium; }
.pool-table td { min-width: 100px; }
.pool-table td:first-child { min-width: 160px; max-width: 240px; }
@media (max-width: 767px) {
  .pool-table, .pool-table tbody { display: block; }
  .pool-table thead { display: none; }
  .pool-table tr { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.75rem; padding: 1rem 0; }
  .pool-table td, .pool-table td:first-child { min-width: 0; max-width: none; padding: 0; overflow-wrap: anywhere; }
  .pool-table td:first-child, .pool-table td:last-child { grid-column: span 2; }
  .pool-table td[data-label]::before { content: attr(data-label); display: block; margin-bottom: 0.25rem; color: #6b7280; }
  :global(.dark) .pool-table td[data-label]::before { color: #9ca3af; }
}
</style>
