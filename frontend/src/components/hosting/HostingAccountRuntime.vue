<template>
  <dl class="grid grid-cols-2 gap-4 rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-800 sm:grid-cols-3">
    <div><dt class="mb-2 text-xs text-gray-500">{{ t('admin.accounts.columns.capacity') }}</dt><dd><HostingAccountCapacity :account="account" /></dd></div>
    <div><dt class="text-xs text-gray-500">{{ t('admin.accounts.priority') }}</dt><dd class="mt-2 font-mono" :title="t('admin.accounts.priorityHint')">{{ account.settings.priority }}</dd></div>
    <div><dt class="text-xs text-gray-500">{{ t('admin.accounts.columns.schedulable') }}</dt><dd class="mt-2"><slot name="scheduling">{{ t(account.schedulable === false ? 'hosting.schedulingOff' : 'hosting.schedulingOn') }}</slot></dd></div>
    <div><dt class="text-xs text-gray-500">{{ t('admin.accounts.columns.platformType') }}</dt><dd class="mt-2"><PlatformTypeBadge platform="openai" type="oauth" :plan-type="account.plan_type" :privacy-mode="String(account.runtime_extra?.privacy_mode ?? '')" /></dd></div>
    <div><dt class="text-xs text-gray-500">{{ t('admin.accounts.expiresAt') }}</dt><dd class="mt-2">{{ account.settings.expires_at ? new Date(account.settings.expires_at * 1000).toLocaleString() : t('hosting.noExpiry') }}</dd></div>
    <div><dt class="text-xs text-gray-500">{{ t('admin.accounts.columns.createdAt') }}</dt><dd class="mt-2">{{ account.created_at ? new Date(account.created_at).toLocaleString() : '—' }}</dd></div>
    <div v-if="account.settings.notes" class="col-span-full"><dt class="text-xs text-gray-500">{{ t('admin.accounts.notes') }}</dt><dd class="mt-2 whitespace-pre-wrap break-words">{{ account.settings.notes }}</dd></div>
  </dl>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { HostedAccount } from '@/api/hosting'
import HostingAccountCapacity from './HostingAccountCapacity.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
defineProps<{ account: HostedAccount }>()
const { t } = useI18n()
</script>
