<template><span v-if="status" class="text-xs" :class="ready ? 'text-emerald-600' : 'text-amber-600'">{{ t(ready ? 'ticketBorrowing.ready' : 'ticketBorrowing.waiting') }}</span></template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useTicketBorrowStatuses } from '@/composables/useTicketBorrowStatuses'
const props = defineProps<{ accountId: number }>()
const { t } = useI18n()
const { rows, now, offset } = useTicketBorrowStatuses()
const status = computed(() => rows.value.find(r => r.account_id === props.accountId && r.enabled))
const ready = computed(() => !!status.value?.ready && Date.parse(status.value.usable_until ?? status.value.expires_at ?? '') > now.value + offset.value)
</script>
