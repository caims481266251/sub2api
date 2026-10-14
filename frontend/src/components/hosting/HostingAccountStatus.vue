<template>
  <AccountStatusIndicator :account="view" :global-bps-enabled="bpsEnabled" @show-temp-unsched="showDetails = true" />
  <TempUnschedStatusModal v-if="showDetails" :show="showDetails" :account="view" :load-status="loadStatus" :recover-account="recover" @close="showDetails = false" />
</template>
<script setup lang="ts">
import { computed, ref } from 'vue'
import AccountStatusIndicator from '@/components/account/AccountStatusIndicator.vue'
import TempUnschedStatusModal from '@/components/account/TempUnschedStatusModal.vue'
import { hostingAPI, type HostedAccount } from '@/api/hosting'
import { hostedAccountView } from './hostedAccountView'
import { useAppStore } from '@/stores/app'
const props = defineProps<{ account: HostedAccount; admin?: boolean }>()
const emit = defineEmits<{ updated: [account: HostedAccount] }>()
const app = useAppStore()
const bpsEnabled = computed(() => app.cachedPublicSettings?.excel_bps_enabled !== false)
const view = computed(() => hostedAccountView(props.account))
const showDetails = ref(false)
const loadStatus = (id: number) => hostingAPI.tempState(id, props.admin)
async function recover(id: number) {
  const updated = await hostingAPI.operation(id, 'recover-state', props.admin)
  emit('updated', updated)
  return hostedAccountView(updated)
}
</script>
