<template>
  <span
    class="inline-flex rounded px-1.5 py-0.5 text-xs"
    :class="status?.ready ? 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200' : 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200'"
  >VM · {{ status?.ready ? t('admin.accounts.vmRuntimeReady') : t('admin.accounts.vmRuntimeError') }}</span>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'
defineProps<{ status?: Account['vm_kernel'] }>()
const { t } = useI18n()
</script>
