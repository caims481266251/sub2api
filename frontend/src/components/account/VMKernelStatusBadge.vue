<template>
  <span
    class="inline-flex rounded px-1.5 py-0.5 text-xs"
    :class="status?.ready ? 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200' : initializing ? 'bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300' : 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200'"
  >Docker · {{ t(status?.ready ? 'admin.accounts.vmRuntimeReady' : initializing ? 'admin.accounts.vmRuntimePending' : 'admin.accounts.vmRuntimeError') }}</span>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { computed } from 'vue'
import type { Account } from '@/types'
const props = defineProps<{ status?: Account['vm_kernel'] }>()
const initializing = computed(() => !props.status || (!props.status.error_code && ['pending', 'preparing', 'create', 'start'].includes(props.status.phase)))
const { t } = useI18n()
</script>
