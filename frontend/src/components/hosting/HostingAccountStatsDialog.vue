<template>
  <AccountStatsModal :show="show && !!account && !details" :account="statsAccount" hosting :load-account-stats="loadStats" @close="emit('close')">
    <template #actions><button class="btn btn-primary" @click="details = true">{{ t('hosting.billingDetails') }}</button></template>
  </AccountStatsModal>
  <HostingBillingDialog :show="show && (!account || details)" :account="account" :admin="admin" @close="closeDetails" />
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AccountStatsModal from '@/components/admin/account/AccountStatsModal.vue'
import HostingBillingDialog from './HostingBillingDialog.vue'
import { hostingAPI, type HostedAccount } from '@/api/hosting'

const props = defineProps<{ show: boolean; account?: HostedAccount | null; admin?: boolean }>()
const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()
const details = ref(false)
const statsAccount = computed(() => props.account ? { id: props.account.id, name: props.account.name, status: props.account.state } : null)
const loadStats = (id: number, days: number) => hostingAPI.accountStats(id, days, props.admin)
watch(() => [props.show, props.account?.id], () => { details.value = false })
function closeDetails() {
  if (props.account) details.value = false
  else emit('close')
}
</script>
