<template>
  <nav :aria-label="t('hosting.adminTitle')" class="flex w-fit max-w-full flex-wrap gap-1 rounded-xl border border-gray-200/70 bg-white/80 p-1 shadow-sm dark:border-dark-700 dark:bg-dark-900">
    <RouterLink v-for="item in items" :key="item.path" :to="item.path" :aria-current="route.path === item.path ? 'page' : undefined" class="rounded-lg px-4 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-primary-500" :class="route.path === item.path ? 'bg-gray-900 font-medium text-white dark:bg-gray-100 dark:text-gray-900' : 'text-gray-500 hover:bg-gray-100 dark:text-gray-400 dark:hover:bg-dark-800'">{{ t(item.label) }}</RouterLink>
  </nav>
</template>
<script setup lang="ts">
import { RouterLink, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
const { t } = useI18n(), route = useRoute()
// Keep the same order as the collapsible hosting group in AppSidebar.vue.
const items = [
  { path: '/admin/hosting/accounts', label: 'hosting.adminAccountsNav' },
  { path: '/admin/hosting/users', label: 'hosting.adminUsersNav' },
  { path: '/admin/hosting/settings', label: 'hosting.adminSettingsNav' },
]
</script>
