<template>
  <p v-if="kind" class="text-xs leading-5 text-gray-500 dark:text-gray-400" data-testid="reason-action">
    {{ t(`ticketBorrowing.management.reasonActions.${kind}`) }}
    <RouterLink v-if="kind === 'proxy' || kind === 'credentials'" :to="kind === 'proxy' ? '/admin/proxies#pool' : '/admin/accounts'" class="ml-2 text-primary-600">{{ t(kind === 'proxy' ? 'ticketBorrowing.managePool' : 'ticketBorrowing.management.manageCredentials') }}</RouterLink>
  </p>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
const props = defineProps<{ code: string }>()
const { t } = useI18n()
const kind = computed(() => /request_budget/.test(props.code) ? 'budget' : /rate_limit|upstream_wait/.test(props.code) ? 'rateLimit' : /credential|auth_excluded|token_revoked/.test(props.code) ? 'credentials' : /proxy_pool|proxy_unavailable/.test(props.code) ? 'proxy' : /ticket_changed|continue_timeout|probe_inconclusive/.test(props.code) ? 'probe' : /unused_after|cancelled|acquisition_paused/.test(props.code) ? 'cancelled' : '')
</script>
