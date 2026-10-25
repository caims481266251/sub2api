<template>
  <div class="hosting-accounts-table flex min-h-0 flex-1 flex-col" data-testid="hosting-accounts-table">
    <DataTable :columns="columns" :data="accounts" :loading="loading" row-key="id" mobile-layout="table" :expandable-actions="false">
      <template #cell-name="{ row }">
        <div class="flex max-w-56 flex-col" data-testid="hosting-account-row">
          <span class="truncate font-medium text-gray-900 dark:text-white" :title="row.name">{{ row.name }}</span>
          <span class="max-w-[200px] truncate text-xs text-gray-500 dark:text-gray-400" :title="row.email">{{ row.email }}</span>
        </div>
      </template>
      <template #cell-id="{ value }"><span class="font-mono text-xs text-gray-500 dark:text-gray-400">#{{ value }}</span></template>
      <template #cell-owner="{ row }"><slot name="owner" :account="row" /></template>
      <template #cell-platform_type="{ row }"><PlatformTypeBadge platform="openai" type="oauth" :plan-type="row.plan_type" :privacy-mode="String(row.runtime_extra?.privacy_mode ?? '')" /></template>
      <template #cell-capacity="{ row }"><HostingAccountCapacity :account="row" /></template>
      <template #cell-status="{ row }"><HostingAccountStatus :account="row" :admin="admin" @updated="emit('updated', $event)" /></template>
      <template #cell-schedulable="{ row }"><AccountSchedulingToggle :model-value="row.schedulable" :account-name="row.name" :disabled="disabled || loading" @update:model-value="emit('schedule', row, $event)" /></template>
      <template #cell-pool="{ row }"><span class="inline-flex max-w-48 truncate rounded-md bg-primary-50 px-2 py-1 text-xs font-medium text-primary-700 dark:bg-primary-900/20 dark:text-primary-400" :title="poolName(row)">{{ poolName(row) }}</span></template>
      <template #cell-usage="{ row }"><div class="min-w-44"><HostingAccountUsage :account="row" :admin="admin" @updated="emit('updated', $event)" /></div></template>
      <template #cell-priority="{ row }"><button type="button" class="rounded px-2 py-1 font-mono hover:bg-gray-100 disabled:opacity-50 dark:hover:bg-dark-700" :disabled="disabled || configurationDisabled" :title="t('admin.accounts.priorityHint')" :aria-label="`${t('admin.accounts.priority')}: ${row.settings.priority}`" @click="emit('edit', row)">{{ row.settings.priority }}</button></template>
      <template #cell-last_used_at="{ value }"><span class="whitespace-nowrap text-xs text-gray-500 dark:text-gray-400">{{ date(value) }}</span></template>
      <template #cell-created_at="{ value }"><span class="whitespace-nowrap text-xs text-gray-500 dark:text-gray-400">{{ date(value) }}</span></template>
      <template #cell-expires_at="{ row }"><span class="whitespace-nowrap text-xs text-gray-500 dark:text-gray-400">{{ row.settings.expires_at ? date(new Date(row.settings.expires_at * 1000).toISOString()) : t('hosting.noExpiry') }}</span><p v-if="row.settings.expires_at && row.settings.auto_pause_on_expired" class="mt-1 text-xs text-gray-500">{{ t('admin.accounts.autoPauseOnExpired') }}</p></template>
      <template #cell-notes="{ row }"><span class="block max-w-56 truncate text-sm text-gray-600 dark:text-gray-300" :title="row.settings.notes || undefined">{{ row.settings.notes || '—' }}</span></template>
      <template #cell-actions="{ row }"><slot name="actions" :account="row" /></template>
      <template #empty><p class="text-sm text-gray-500 dark:text-gray-400">{{ emptyText }}</p></template>
    </DataTable>
  </div>
</template>

<script setup lang="ts" generic="T extends HostedAccount">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { HostedAccount } from '@/api/hosting'
import DataTable from '@/components/common/DataTable.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
import AccountSchedulingToggle from '@/components/account/AccountSchedulingToggle.vue'
import HostingAccountCapacity from './HostingAccountCapacity.vue'
import HostingAccountStatus from './HostingAccountStatus.vue'
import HostingAccountUsage from './HostingAccountUsage.vue'

const props = defineProps<{ accounts: T[]; loading?: boolean; disabled?: boolean; configurationDisabled?: boolean; admin?: boolean; poolName: (account: T) => string; emptyText: string }>()
const emit = defineEmits<{ updated: [account: HostedAccount]; schedule: [account: T, enabled: boolean]; edit: [account: T] }>()
const { t } = useI18n()
const columns = computed(() => [
  { key: 'name', label: t('admin.accounts.columns.name') },
  { key: 'id', label: t('admin.accounts.columns.id') },
  ...(props.admin ? [{ key: 'owner', label: t('hosting.ownerUser') }] : []),
  { key: 'platform_type', label: t('admin.accounts.columns.platformType') },
  { key: 'capacity', label: t('admin.accounts.columns.capacity') },
  { key: 'status', label: t('admin.accounts.columns.status') },
  { key: 'schedulable', label: t('admin.accounts.columns.schedulable') },
  { key: 'pool', label: t('hosting.pool') },
  { key: 'usage', label: t('admin.accounts.columns.usageWindows') },
  { key: 'priority', label: t('admin.accounts.priority') },
  { key: 'last_used_at', label: t('admin.accounts.columns.lastUsed') },
  { key: 'created_at', label: t('admin.accounts.columns.createdAt') },
  { key: 'expires_at', label: t('admin.accounts.columns.expiresAt') },
  { key: 'notes', label: t('admin.accounts.columns.notes') },
  { key: 'actions', label: t('admin.accounts.columns.actions') }
])
const date = (value?: string | null) => value ? new Date(value).toLocaleString() : '—'
</script>

<style scoped>
/* Keep the user's row layout without pinned columns covering a small screen. */
@media (max-width: 767px) {
  .hosting-accounts-table :deep(.sticky-col) { position: static; }
  .hosting-accounts-table :deep(.sticky-col::before),
  .hosting-accounts-table :deep(.sticky-col::after) { display: none; }
}
</style>
