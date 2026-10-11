<template>
  <section class="rounded-lg border border-gray-100 p-3 dark:border-dark-700" data-testid="business-routes">
    <div class="flex flex-wrap items-center justify-between gap-2 text-sm">
      <h3 class="font-medium">{{ t('ticketBorrowing.routes.title') }}</h3>
      <span class="tabular-nums" data-testid="business-route-load">{{ t('ticketBorrowing.routes.load', { count: target.business_inflight ?? 0, limit: target.business_concurrency ?? '—' }) }}</span>
    </div>
    <p class="mt-1 text-xs text-gray-500">{{ t('ticketBorrowing.routes.hint') }}</p>
    <div v-if="target.business_routes?.length" class="mt-3 overflow-x-auto">
      <table class="w-full text-left text-xs">
        <thead class="text-gray-500"><tr>
          <th class="py-2 pr-3">{{ t('ticketBorrowing.routes.route') }}</th>
          <th class="py-2 pr-3">{{ t('ticketBorrowing.routes.state') }}</th>
          <th class="py-2 pr-3">{{ t('ticketBorrowing.routes.remaining') }}</th>
          <th class="py-2">{{ t('ticketBorrowing.routes.inflight') }}</th>
        </tr></thead>
        <tbody>
          <tr v-for="route in target.business_routes" :key="route.id" :data-business-route="route.id" class="border-t border-gray-100 dark:border-dark-700">
            <td class="py-2 pr-3"><div class="break-all font-mono">{{ route.gateway }}</div><div class="mt-1 text-gray-500" :title="route.id">{{ route.id.slice(0, 8) }} · {{ t('ticketBorrowing.routes.source', { id: route.source_account_id }) }}</div></td>
            <td class="whitespace-nowrap py-2 pr-3">{{ t(`ticketBorrowing.routes.${state(route)}`) }}</td>
            <td class="whitespace-nowrap py-2 pr-3 tabular-nums">{{ t('ticketBorrowing.seconds', { count: seconds(route.usable_until) }) }}</td>
            <td class="py-2 tabular-nums" data-route-inflight>{{ route.inflight }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="mt-3 text-sm text-gray-500">{{ t('ticketBorrowing.routes.empty') }}</p>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { BorrowRouteStatus, BorrowTarget } from '@/api/admin/ticketBorrowing'

const props = defineProps<{ target: BorrowTarget; now: number }>()
const { t } = useI18n()
function seconds(time: string) { return Math.max(0, Math.ceil((Date.parse(time) - props.now) / 1000)) }
function state(route: BorrowRouteStatus) {
  if (!props.target.enabled || !route.usable || seconds(route.usable_until) === 0) return route.inflight > 0 ? 'draining' : 'unavailable'
  return route.inventory_eligible ? 'accepting' : 'boundOnly'
}
</script>
