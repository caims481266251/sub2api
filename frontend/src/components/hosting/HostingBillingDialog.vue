<template>
  <BaseDialog :show="show" :title="t('admin.accounts.usageStatistics')" width="extra-wide" @close="emit('close')">
    <div class="space-y-5">
      <div class="flex items-center gap-3 rounded-xl border border-primary-200 bg-gradient-to-r from-primary-50 to-primary-100 p-3 dark:border-primary-700/50 dark:from-primary-900/20 dark:to-primary-800/20"><Icon name="chart" class="text-primary-600" /><div><p class="font-semibold">{{ account?.name || t('hosting.title') }}</p><p class="text-xs text-gray-500">{{ t('hosting.billingHistoryHint') }}</p></div></div>
      <div v-if="!account" class="flex flex-wrap gap-2"><button v-for="choice in routes" :key="choice.value" class="btn" :class="route === choice.value ? 'btn-primary' : 'btn-secondary'" @click="route = choice.value; load(1)">{{ t(choice.label) }}</button></div>
      <p v-if="error" role="alert" class="text-red-600">{{ error }}<button class="btn btn-secondary ml-3" @click="load(page)">{{ t('common.refresh') }}</button></p>
      <p v-if="loading" role="status" class="py-6 text-center text-gray-500">{{ t('common.loading') }}</p>
      <template v-else-if="data">
        <div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
          <div class="card border-emerald-200 bg-gradient-to-br from-emerald-50 to-white p-4 dark:border-emerald-800/30 dark:from-emerald-900/10 dark:to-dark-700"><p class="text-xs text-gray-500">{{ t('hosting.actualDebit') }}</p><p class="mt-2 text-2xl font-bold">{{ money(data.actual_cost) }}</p></div>
          <div class="card p-4"><p class="text-xs text-gray-500">{{ t('hosting.standardCost') }}</p><p class="mt-2 text-xl font-semibold">{{ money(data.standard_cost) }}</p></div>
          <div class="card p-4"><p class="text-xs text-gray-500">{{ t('hosting.billingRequests') }}</p><p class="mt-2 text-xl font-semibold">{{ data.requests.toLocaleString() }}</p></div>
          <div class="card p-4"><p class="text-xs text-gray-500">Tokens</p><p class="mt-2 text-xl font-semibold">{{ data.tokens.toLocaleString() }}</p></div>
        </div>
        <p class="text-sm text-gray-500">{{ t('hosting.hostingCost') }}: {{ money(data.hosting_cost) }} · {{ t('hosting.fallbackCost') }}: {{ money(data.fallback_cost) }}</p>
        <div class="max-h-[45vh] overflow-auto"><table class="w-full min-w-[1000px] text-left text-sm"><thead class="sticky top-0 bg-gray-50 dark:bg-dark-800"><tr><th>{{ t('hosting.billingTime') }}</th><th>{{ t('hosting.billingSource') }}</th><th>{{ t('hosting.accountName') }} / Key</th><th>{{ t('hosting.billingModel') }}</th><th>Tokens</th><th>{{ t('hosting.billingBase') }}</th><th>{{ t('hosting.billingRate') }}</th><th>{{ t('hosting.actualDebit') }}</th></tr></thead><tbody>
          <tr v-if="!data.items.length"><td colspan="8" class="py-10 text-center text-gray-500">{{ t('hosting.noBillingRecords') }}</td></tr>
          <template v-for="item in data.items" :key="item.id"><tr class="border-t border-gray-100 dark:border-dark-700"><td class="whitespace-nowrap">{{ new Date(item.created_at).toLocaleString() }}</td><td><span class="rounded px-2 py-1 text-xs" :class="item.route === 'hosted' ? 'bg-teal-50 text-teal-700' : 'bg-amber-50 text-amber-700'">{{ t(item.route === 'hosted' ? 'hosting.hostedUsage' : 'hosting.fallbackUsage') }}</span><p v-if="item.group_name" class="mt-2 text-xs">{{ item.group_name }}</p><p v-if="item.reason" class="mt-1 max-w-52 text-xs text-gray-500">{{ item.reason }}</p></td><td><p>{{ item.account_name }}</p><p class="mt-1 text-xs text-gray-500">{{ item.key_name }}</p></td><td>{{ item.model }}</td><td>{{ item.tokens.toLocaleString() }}</td><td>{{ money(item.standard_cost) }}</td><td>{{ item.rate }}×</td><td class="whitespace-nowrap"><p class="font-semibold text-green-600 dark:text-green-400">{{ money(item.actual_cost) }}</p><button class="mt-1 text-xs text-primary-600" :aria-expanded="expandedId === item.id" @click="expandedId = expandedId === item.id ? null : item.id">{{ t('hosting.billingDetails') }}</button></td></tr>
          <tr v-if="expandedId === item.id" class="bg-gray-50 dark:bg-dark-800"><td colspan="8"><div class="space-y-3 p-3 text-xs">
            <p class="font-mono">{{ t('hosting.billingRequestId') }}: {{ item.request_id || '—' }}</p>
            <div class="grid grid-cols-2 gap-3 lg:grid-cols-4"><div v-for="part in costParts" :key="part.cost"><p class="text-gray-500">{{ t(part.label) }}</p><p class="mt-1 font-mono">{{ (item[part.tokens] ?? 0).toLocaleString() }} tokens · {{ money(item[part.cost] ?? 0) }}</p></div></div>
            <p>{{ t('hosting.billingBase') }}: {{ money(item.standard_cost) }} · {{ t('hosting.billingRate') }}: {{ item.rate }}× · {{ t(item.route === 'hosted' ? 'hosting.hostedDebit' : 'hosting.fallbackDebit') }}: <strong>{{ money(item.actual_cost) }}</strong></p>
            <p class="text-gray-500">{{ t('hosting.billingPrecisionHint') }}</p>
          </div></td></tr></template>
        </tbody></table></div>
        <Pagination :page="page" :page-size="pageSize" :total="data.requests" @update:page="load" @update:page-size="pageSize = $event; load(1)" />
      </template>
    </div>
  </BaseDialog>
</template>
<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatCostFixed } from '@/utils/format'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import Icon from '@/components/icons/Icon.vue'
import { hostingAPI, hostingErrorMessage, type HostedAccount, type HostingBilling } from '@/api/hosting'
const props = defineProps<{ show: boolean; account?: HostedAccount | null; admin?: boolean }>()
const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()
const data = ref<HostingBilling>(), route = ref(''), page = ref(1), pageSize = ref(20), loading = ref(false), error = ref('')
const routes = [{ value: '', label: 'hosting.allBilling' }, { value: 'hosted', label: 'hosting.hostedUsage' }, { value: 'fallback', label: 'hosting.fallbackUsage' }]
const money = (value: number) => `${formatCostFixed(value, 8)} USD`
const expandedId = ref<number | null>(null)
const costParts = [
  { cost: 'input_cost', tokens: 'input_tokens', label: 'hosting.billingInput' },
  { cost: 'output_cost', tokens: 'output_tokens', label: 'hosting.billingOutput' },
  { cost: 'cache_read_cost', tokens: 'cache_read_tokens', label: 'hosting.billingCacheRead' },
  { cost: 'cache_creation_cost', tokens: 'cache_creation_tokens', label: 'hosting.billingCacheWrite' },
] as const
let generation = 0
async function load(targetPage = 1) {
  const current = ++generation
  expandedId.value = null
  loading.value = true; error.value = ''; data.value = undefined; page.value = targetPage
  try { const result = await hostingAPI.billing(props.account?.id ?? 0, route.value, targetPage, pageSize.value, props.admin); if (current === generation) data.value = result }
  catch (e) { if (current === generation) error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { if (current === generation) loading.value = false }
}
watch(() => [props.show, props.account?.id, props.admin], () => { generation++; data.value = undefined; if (props.show) { route.value = ''; void load(1) } }, { immediate: true })
</script>
