<template>
  <AppLayout>
    <TablePageLayout>
      <template #actions>
        <div class="space-y-4">
          <header><h1 class="text-2xl font-semibold">{{ t('hosting.adminAccountsNav') }}</h1><p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('hosting.adminAccountsDescription') }}</p></header>
          <HostingAdminNav />
        </div>
      </template>
      <template #filters>
        <form id="hosting-account-search-form" class="flex flex-wrap items-center gap-2" @submit.prevent="applySearch">
          <div class="relative min-w-48 flex-1 sm:max-w-sm"><label for="hosting-account-search" class="sr-only">{{ t('common.search') }}</label><Icon name="search" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" /><input id="hosting-account-search" v-model="searchDisplay" class="input w-full pl-9" maxlength="200" :placeholder="t('hosting.adminAccountSearch')" /></div>
          <div class="w-24"><label for="hosting-account-page-size" class="sr-only">{{ t('pagination.perPage') }}</label><Select id="hosting-account-page-size" :model-value="pageSize" :options="pageSizes" :aria-label="t('pagination.perPage')" data-testid="hosting-page-size" @update:model-value="changePageSize" /></div>
          <button class="btn btn-primary" :disabled="loading">{{ t('common.search') }}</button>
          <button v-if="appliedSearch || searchDraft" type="button" class="btn btn-secondary" :disabled="loading" @click="resetSearch">{{ t('hosting.clearSearch') }}</button>
          <button type="button" class="btn btn-secondary" :disabled="loading" @click="load(page)">{{ t('common.refresh') }}</button>
        </form>
        <p class="mt-2 text-xs text-gray-500" aria-live="polite">{{ t('hosting.adminAccountsHint', { count: total }) }}<span v-if="appliedSearch" class="ml-2">{{ t('hosting.searchApplied', { search: appliedSearchDisplay }) }}</span></p>
        <p v-if="error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      </template>
      <template #table>
        <HostingAccountsTable :accounts="accounts" :loading="loading" :disabled="busy" admin :pool-name="account => account.pool_name || t('hosting.defaultPool')" :empty-text="t('hosting.adminAccountsEmpty')" @updated="updateAccount" @edit="editAccount" @schedule="(account, enabled) => changeAccount(account, enabled ? 'schedule-enable' : 'schedule-disable')">
          <template #owner="{ account }"><button type="button" class="text-left font-medium text-primary-600 hover:underline dark:text-primary-400" :title="t('hosting.viewUserAccounts')" @click="filterOwner(account.user_id)">{{ account.username || account.user_email }}</button><p class="mt-1 max-w-48 truncate text-xs text-gray-500" :title="account.user_email">{{ account.user_email }} · #{{ account.user_id }}</p></template>
          <template #actions="{ account }"><HostingAccountActions @reload="load" :account="account" @updated="updateAccount" @stats="billingAccount = account; showBilling = true" :disabled="busy" admin @edit="editAccount(account)" @delete="deleteTarget = account" @toggle="changeAccount(account, account.state === 'paused' ? 'resume' : 'pause')" @refresh-usage="refreshUsage(account)" @policy="policyUser = { id: account.user_id, username: account.username, email: account.user_email }" /></template>
        </HostingAccountsTable>
      </template>
      <template #pagination><Pagination v-if="total > 0" :page="page" :page-size="pageSize" :show-page-size-selector="false" show-jump :total="total" @update:page="load" /></template>
    </TablePageLayout>
    <HostingAccountSettingsDialog :account="settingsTarget" :pools="accountPools" admin @close="settingsTarget = null" @saved="load(page)" />
    <ConfirmDialog :show="!!deleteTarget" :title="t('hosting.deleteTitle')" :message="t('hosting.deleteHint')" danger @cancel="deleteTarget = null" @confirm="confirmDelete" />
    <HostingUserPolicyDialog :user="policyUser" @close="policyUser = null" />
  <HostingAccountStatsDialog :show="showBilling" :account="billingAccount" admin @close="showBilling = false" />
  </AppLayout>
</template>
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import HostingAccountStatsDialog from '@/components/hosting/HostingAccountStatsDialog.vue'
import HostingAccountsTable from '@/components/hosting/HostingAccountsTable.vue'
import Icon from '@/components/icons/Icon.vue'
import HostingAccountActions from '@/components/hosting/HostingAccountActions.vue'
import HostingAccountSettingsDialog from '@/components/hosting/HostingAccountSettingsDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import HostingAdminNav from '@/components/admin/hosting/HostingAdminNav.vue'
import HostingUserPolicyDialog from '@/components/admin/hosting/HostingUserPolicyDialog.vue'
import { hostingAPI, hostingErrorMessage, type HostingAdminAccount, type HostingAccountAction, type HostedAccount, type HostingPool } from '@/api/hosting'
const showBilling = ref(false), billingAccount = ref<HostedAccount | null>(null)
const { t } = useI18n(), route = useRoute(), router = useRouter()
const accounts = ref<HostingAdminAccount[]>([]), searchDraft = ref(''), appliedSearch = ref('')
const ownerFilter = ref<{ id: string; label: string } | null>(null)
const searchDisplay = computed({
  get: () => ownerFilter.value?.id === searchDraft.value ? ownerFilter.value.label : searchDraft.value,
  set: (value: string) => { searchDraft.value = value; ownerFilter.value = null },
})
const appliedSearchDisplay = computed(() => ownerFilter.value?.id === appliedSearch.value ? ownerFilter.value.label : appliedSearch.value)
const page = ref(1), pageSize = ref(20), total = ref(0), loading = ref(false), error = ref('')
const policyUser = ref<{ id: number; username: string; email: string } | null>(null)
const settingsTarget = ref<HostingAdminAccount | null>(null), deleteTarget = ref<HostingAdminAccount | null>(null)
const accountPools = ref<HostingPool[]>([]), busy = ref(false)
const pageSizes = [20, 50, 100].map(value => ({ value, label: String(value) }))
function updateAccount(account: HostedAccount) {
  accounts.value = accounts.value.map(item => item.id === account.id ? { ...item, ...account } : item)
}
async function refreshUsage(account: HostedAccount) {
  if (busy.value) return
  busy.value = true; error.value = ''
  try { const usage = await hostingAPI.accountUsage(account.id, true, true); updateAccount({ ...account, usage }) }
  catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { busy.value = false }
}
async function editAccount(account: HostingAdminAccount) {
  if (busy.value) return
  busy.value = true; error.value = ''
  try { accountPools.value = await hostingAPI.adminAccountPools(account.id); settingsTarget.value = account }
  catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { busy.value = false }
}
async function changeAccount(account: HostingAdminAccount, action: HostingAccountAction) {
  if (busy.value) return
  busy.value = true; error.value = ''
  try { await hostingAPI.change(account.id, action, true); deleteTarget.value = null; await load(page.value) }
  catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { busy.value = false }
}
function confirmDelete() { if (deleteTarget.value) void changeAccount(deleteTarget.value, 'delete') }
let request = 0
async function load(targetPage = 1) {
  const current = ++request
  loading.value = true; error.value = ''
  try {
    const result = await hostingAPI.adminAccounts(targetPage, pageSize.value, appliedSearch.value)
    if (current !== request) return
    const lastPage = Math.max(1, Math.ceil(result.total / pageSize.value))
    if (targetPage > lastPage) { await load(lastPage); return }
    accounts.value = result.items; total.value = result.total; page.value = result.page
    if (ownerFilter.value) {
      const owner = result.items.find(item => String(item.user_id) === ownerFilter.value?.id)
      if (owner) ownerFilter.value.label = owner.user_email || owner.username || `#${owner.user_id}`
    }
  } catch (e) { if (current === request) error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { if (current === request) loading.value = false }
}
function applySearch() {
  appliedSearch.value = searchDraft.value.trim()
  const id = ownerFilter.value?.id === appliedSearch.value ? ownerFilter.value.id : undefined
  if (route.query.user_id !== id) {
    const { user_id: _userID, ...query } = route.query
    void router.replace({ query: { ...query, ...(id ? { user_id: id } : {}) } })
  }
  void load(1)
}
function resetSearch() {
  ownerFilter.value = null; searchDraft.value = ''; applySearch()
}
function filterOwner(id: number) {
  const owner = accounts.value.find(item => item.user_id === id)
  ownerFilter.value = { id: String(id), label: owner?.user_email || owner?.username || `#${id}` }
  searchDraft.value = String(id); applySearch()
}
function changePageSize(value: unknown) { const size = Number(value); if (![20, 50, 100].includes(size)) return; pageSize.value = size; void load(1) }
watch(() => route.query.user_id, value => {
  const id = typeof value === 'string' && /^[1-9]\d*$/.test(value) ? value : ''
  if (request > 0 && id === (ownerFilter.value?.id ?? '') && appliedSearch.value === searchDraft.value.trim()) return
  ownerFilter.value = id ? { id, label: `#${id}` } : null
  searchDraft.value = id; appliedSearch.value = id; void load(1)
}, { immediate: true })
</script>
