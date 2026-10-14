<template>
  <OpenAIUsagePanel
    :account="quotaAccount"
    :usage="usage"
    :active-query-loading="loading"
    :error="error"
    :transport="transport"
    :show-referral="false"
    @refresh="refresh"
  />
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { hostingAPI, hostingErrorMessage, type HostedAccount } from '@/api/hosting'
import type { AccountUsageInfo } from '@/types'
import OpenAIUsagePanel from '@/components/account/OpenAIUsagePanel.vue'
import type { OpenAIQuotaAccount, OpenAIQuotaTransport } from '@/components/account/openAIQuotaTransport'

const props = withDefaults(defineProps<{ account: HostedAccount; admin?: boolean }>(), { admin: false })
const emit = defineEmits<{ updated: [account: HostedAccount] }>()
const { t } = useI18n()
const usage = ref<AccountUsageInfo | null>(null)
const loading = ref(false), error = ref<string | null>(null)
let generation = 0
let disposed = false
const quotaAccount = computed<OpenAIQuotaAccount>(() => ({
  id: props.account.id, name: props.account.name, platform: 'openai', type: 'oauth',
  extra: props.account.quota_extra,
}))

// Isolate requests from the admin API and discard results from an old account.
watch(() => [props.account.id, props.admin, props.account.usage, props.account.usage_5h_percent, props.account.usage_7d_percent], () => {
  generation++
  error.value = null; loading.value = false
  const window = (percent: number | null) => percent == null ? null : { utilization: percent, resets_at: null, remaining_seconds: 0 }
  usage.value = props.account.usage ?? { updated_at: null, seven_day_sonnet: null, five_hour: window(props.account.usage_5h_percent), seven_day: window(props.account.usage_7d_percent) }
}, { immediate: true })
onBeforeUnmount(() => { disposed = true; generation++ })

async function refresh() {
  if (loading.value) return
  const current = ++generation
  loading.value = true; error.value = null
  try {
    const result = await hostingAPI.accountUsage(props.account.id, true, props.admin)
    if (current === generation) usage.value = result
  } catch (e) {
    if (current === generation) error.value = hostingErrorMessage(e, t('hosting.operationFailed'))
  } finally { if (current === generation) loading.value = false }
}

const transport: OpenAIQuotaTransport = {
  refresh: id => hostingAPI.refreshQuota(id, props.admin),
  reset: async id => {
    const admin = props.admin
    const { account, ...result } = await hostingAPI.resetQuota(id, admin)
    if (!disposed && id === props.account.id && admin === props.admin && account) emit('updated', account)
    return result
  },
}
</script>
