<template>
  <section class="rounded-lg border border-gray-100 p-3 dark:border-dark-700" data-testid="business-routes">
    <div class="flex flex-wrap items-center justify-between gap-2 text-sm">
      <h3 class="font-medium">{{ t('ticketBorrowing.routes.title') }}</h3>
      <span class="tabular-nums" data-testid="business-route-load">{{ t('ticketBorrowing.routes.load', { count: target.business_inflight ?? 0, limit: target.business_concurrency ?? '—' }) }}</span>
    </div>
    <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.routes.hint') }}</p>
    <div v-if="target.business_routes?.length" class="mt-3 overflow-x-auto">
      <table class="borrow-routes-table w-full text-left text-xs">
        <thead class="text-gray-500 dark:text-gray-400"><tr>
          <th class="py-2 pr-3">{{ t('ticketBorrowing.routes.route') }}</th>
          <th class="py-2 pr-3">{{ t('ticketBorrowing.routes.state') }}</th>
          <th class="py-2 pr-3">{{ t('ticketBorrowing.routes.remaining') }}</th>
          <th class="py-2">{{ t('ticketBorrowing.routes.inflight') }}</th><th class="py-2">{{ t('ticketBorrowing.routes.times') }}</th>
        </tr></thead>
        <tbody>
          <tr v-for="route in target.business_routes" :key="route.id" :data-business-route="route.id" class="border-t border-gray-100 dark:border-dark-700">
            <td class="py-2 pr-3"><div class="break-all font-mono">{{ route.gateway }}</div><BorrowTag v-if="route.id === target.preferred_route_id && state(route) === 'accepting'" class="mt-1" kind="state" value="preferred" data-testid="preferred-route" /><div class="mt-1 text-gray-500 dark:text-gray-400" :title="route.id">{{ route.id.slice(0, 8) }} · {{ t('ticketBorrowing.routes.source', { id: route.source_account_id }) }}</div></td>
            <td :data-label="t('ticketBorrowing.routes.state')" class="py-2 pr-3"><BorrowTag kind="route" :value="state(route)" /></td>
            <td :data-label="t('ticketBorrowing.routes.remaining')" class="whitespace-nowrap py-2 pr-3 tabular-nums">{{ t('ticketBorrowing.seconds', { count: seconds(route.usable_until) }) }}</td>
            <td :data-label="t('ticketBorrowing.routes.inflight')" class="py-2 tabular-nums" data-route-inflight>{{ route.inflight }}</td>
            <td class="py-2 space-y-1"><p>{{ t('ticketBorrowing.routes.published') }}: {{ date(route.published_at) }}</p><p>{{ t('ticketBorrowing.routes.admissionDeadline') }}: {{ date(route.usable_until) }}</p><p>{{ t('ticketBorrowing.routes.ticketExpiry') }}: {{ date(route.expires_at) }}</p><button class="text-primary-600" @click="$emit('records', route.id)">{{ t('ticketBorrowing.management.relatedRecords') }}</button></td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="mt-3 text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.routes.empty') }}</p>
  </section>
</template>

<script setup lang="ts">
import BorrowTag from './BorrowTag.vue'
import { useI18n } from 'vue-i18n'
import type { BorrowRouteStatus, BorrowTarget } from '@/api/admin/ticketBorrowing'

const props = defineProps<{ target: BorrowTarget; now: number }>()
defineEmits<{ records: [routeId: string] }>()
const { t } = useI18n()
function date(value?: string) { return value ? new Date(value).toLocaleTimeString() : '—' }
function seconds(time: string) { return Math.max(0, Math.ceil((Date.parse(time) - props.now) / 1000)) }
function state(route: BorrowRouteStatus) {
  if (!props.target.enabled || !route.usable || seconds(route.usable_until) === 0) return route.inflight > 0 ? 'draining' : 'unavailable'
  return route.inventory_eligible ? 'accepting' : 'boundOnly'
}
</script>

<style scoped>
@media (max-width: 767px) {
 .borrow-routes-table, .borrow-routes-table tbody { display: block; }
 .borrow-routes-table thead { display: none; }
 .borrow-routes-table tr { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.5rem 1rem; padding: 0.75rem 0; }
 .borrow-routes-table td { min-width: 0; padding: 0; }
 .borrow-routes-table td:first-child, .borrow-routes-table td:last-child { grid-column: span 2; }
 .borrow-routes-table td[data-label]::before { content: attr(data-label); display: block; margin-bottom: 0.25rem; color: #6b7280; }
 :global(.dark) .borrow-routes-table td[data-label]::before { color: #9ca3af; }
}
</style>
