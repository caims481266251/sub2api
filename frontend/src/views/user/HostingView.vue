<template>
  <AppLayout>
    <section class="space-y-6">
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div><h1 class="text-2xl font-semibold">{{ t('hosting.title') }}</h1><p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('hosting.description') }}</p></div>
        <div class="flex flex-wrap gap-3"><button class="btn btn-secondary min-h-11" :disabled="loading" @click="load">{{ t('common.refresh') }}</button><button class="btn btn-secondary min-h-11" :disabled="!canAdd" @click="openImport()">{{ t('hosting.importAccount') }}</button><button class="btn btn-primary min-h-11" :disabled="!canAdd" @click="openAdd()">{{ t('hosting.addAccount') }}</button></div>
      </header>
      <p v-if="error" role="alert" class="rounded-lg bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300">{{ error }}</p>
      <p v-if="overview && !overview.enabled" class="rounded-lg bg-amber-50 p-4 text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">{{ t('hosting.disabled') }}</p>
      <div v-if="overview" class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <div class="card p-5"><p class="text-sm text-gray-500">{{ t('hosting.seats') }}</p><p class="mt-2 text-2xl font-semibold">{{ overview.used_seats }} / {{ overview.seat_limit }}</p><p class="mt-2 text-xs text-gray-500">{{ t('hosting.seatsHint') }}</p></div>
        <div class="card p-5"><p class="text-sm text-gray-500">{{ t('hosting.currentRate') }}</p><p class="mt-2 text-2xl font-semibold">{{ overview.rate_multiplier }}×</p><p class="mt-2 text-xs text-gray-500">{{ t('hosting.rateHint') }}</p></div>
        <div class="card p-5"><p class="text-sm text-gray-500">{{ t('hosting.hostingCost') }}</p><p class="mt-2 break-all text-xl font-semibold">{{ money(overview.summary.hosting_cost) }}</p><p class="mt-2 text-xs text-gray-500">{{ t('hosting.standardCost') }}: {{ money(overview.summary.standard_cost) }}</p></div>
        <div class="card p-5"><p class="text-sm text-gray-500">{{ t('hosting.fallbackCost') }}</p><p class="mt-2 break-all text-xl font-semibold">{{ money(overview.summary.fallback_cost) }}</p><button class="mt-2 inline-block text-sm text-primary-600 dark:text-primary-400" @click="billingAccount = null; showBilling = true">{{ t('hosting.viewUsage') }}</button></div>
      </div>
      <div class="card p-5"><p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t('hosting.billingExplanation') }}</p><RouterLink to="/keys" class="mt-3 inline-block text-sm font-medium text-primary-600 dark:text-primary-400">{{ t('hosting.configureKey') }}</RouterLink></div>
      <section v-if="overview" class="space-y-3">
        <div class="flex flex-wrap items-center gap-2" data-testid="hosting-account-toolbar">
          <div class="relative min-w-48 flex-1 sm:max-w-xs">
            <label for="hosting-account-search" class="sr-only">{{ t('common.search') }}</label>
            <Icon name="search" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
            <input id="hosting-account-search" v-model="accountSearch" type="search" class="input pl-9" :placeholder="t('hosting.userAccountSearch')" />
          </div>
          <div class="w-36"><label for="hosting-account-state" class="sr-only">{{ t('common.status') }}</label><select id="hosting-account-state" v-model="accountState" class="input"><option value="">{{ t('hosting.allStates') }}</option><option v-for="state in accountStates" :key="state" :value="state">{{ t(`hosting.states.${state}`) }}</option></select></div>
          <div class="w-48"><label for="hosting-pool-filter" class="sr-only">{{ t('hosting.pool') }}</label><select id="hosting-pool-filter" v-model.number="selectedPoolId" class="input"><option :value="-1">{{ t('hosting.allPools') }}</option><option v-for="pool in overview.pools" :key="pool.id" :value="pool.id">{{ poolName(pool.id) }} · {{ t('hosting.poolCounts', { accounts: pool.account_count, keys: pool.key_count }) }}</option></select></div>
          <button v-if="accountSearch || accountState || selectedPoolId >= 0" type="button" class="btn btn-secondary" @click="accountSearch = ''; accountState = ''; selectedPoolId = -1">{{ t('hosting.clearSearch') }}</button>
          <button class="btn btn-secondary" :disabled="busy || !overview.enabled" @click="openPool()"><Icon name="plus" size="sm" class="mr-1.5" />{{ t('hosting.createPool') }}</button>
          <template v-if="selectedPoolId > 0"><button class="btn btn-secondary" :disabled="busy || !overview.enabled" @click="openPool(selectedPool)">{{ t('hosting.renamePool') }}</button><button class="btn btn-secondary text-red-600 dark:text-red-400" :disabled="busy || !!selectedPool?.account_count || !!selectedPool?.key_count" @click="deletePoolTarget = selectedPool ?? null">{{ t('common.delete') }}</button></template>
          <span class="ml-auto text-xs text-gray-500" aria-live="polite">{{ t('hosting.filteredCount', { count: filteredAccounts.length }) }}</span>
        </div>
        <details class="text-xs text-gray-500 dark:text-gray-400"><summary class="w-fit cursor-pointer hover:text-primary-600">{{ t('hosting.poolsTitle') }}</summary><p class="mt-2">{{ t('hosting.poolsHint') }}</p></details>
        <div class="card flex flex-col overflow-hidden">
          <HostingAccountsTable class="max-h-[65vh]" :accounts="pagedAccounts" :loading="loading" :disabled="busy" :configuration-disabled="!overview.enabled" :pool-name="account => poolName(account.settings.hosting_pool_id ?? 0)" :empty-text="t(accountSearch || accountState ? 'hosting.noMatchingAccounts' : selectedPoolId >= 0 ? 'hosting.emptyPool' : 'hosting.empty')" @updated="updateAccount" @edit="settingsTarget = $event" @schedule="(account, enabled) => change(account, enabled ? 'schedule-enable' : 'schedule-disable')">
            <template #actions="{ account }"><HostingAccountActions @reload="load" :account="account" @updated="updateAccount" @stats="billingAccount = account; showBilling = true" :disabled="busy" :configuration-disabled="!overview.enabled" @edit="settingsTarget = account" @delete="deleteTarget = account" @reauth="openAdd(account)" @toggle="change(account, account.state === 'paused' ? 'resume' : 'pause')" @refresh-usage="refreshUsage(account)" /></template>
          </HostingAccountsTable>
          <Pagination v-if="filteredAccounts.length" v-model:page="accountPage" v-model:page-size="accountPageSize" :total="filteredAccounts.length" />
        </div>
      </section>
      <p v-else-if="loading" role="status">{{ t('common.loading') }}</p>
      <BaseDialog :show="showAdd" :title="dialogTitle" width="wide" @close="closeAdd">
        <ol class="mb-6 flex items-center justify-center gap-4 text-sm" :aria-label="dialogTitle">
          <li class="flex items-center gap-2"><span class="flex h-8 w-8 items-center justify-center rounded-full bg-primary-500 font-semibold text-white">1</span><span>{{ t('hosting.accountSettings') }}</span></li>
          <li aria-hidden="true" class="h-0.5 w-8 bg-gray-300 dark:bg-dark-600" />
          <li class="flex items-center gap-2"><span class="flex h-8 w-8 items-center justify-center rounded-full font-semibold" :class="addStep === 2 ? 'bg-primary-500 text-white' : 'bg-gray-200 text-gray-500 dark:bg-dark-600'">2</span><span>{{ t(isImport ? 'hosting.importCredentials' : 'hosting.authorization') }}</span></li>
        </ol>
        <form id="hosting-add-form" class="space-y-5" @submit.prevent="addStep === 1 ? nextAddStep() : submit()">
          <div v-if="showAdd" v-show="addStep === 1" :hidden="addStep !== 1" class="space-y-5" data-testid="hosting-add-configuration">
            <HostingAccountSettingsForm ref="addSettingsForm" :settings="addInitialSettings" :pools="overview?.pools" :disabled="busy" :pool-disabled="!!replaceId">
              <template #identity>
                <div><label class="input-label">{{ t('admin.accounts.platform') }}</label><div class="mt-2 flex rounded-lg bg-gray-100 p-1 dark:bg-dark-700"><div class="flex flex-1 items-center justify-center gap-2 rounded-md bg-white px-4 py-2.5 text-sm font-medium text-green-600 shadow-sm dark:bg-dark-600 dark:text-green-400"><Icon name="bolt" size="sm" />OpenAI</div></div></div>
                <div v-if="!isImport || replaceId"><label class="input-label">{{ t(replaceId ? 'hosting.reauthorizeMethod' : 'hosting.addMethod') }}</label><div class="mt-2 grid grid-cols-1 gap-3" :class="replaceId ? 'sm:grid-cols-3' : 'sm:grid-cols-2'">
                  <button v-for="choice in addMethods" :key="choice.value" type="button" :disabled="busy || (choice.value === 'two_fa' && !overview?.two_fa_login_available)" :aria-pressed="method === choice.value" :data-testid="`hosting-method-${choice.value}`" :class="['flex items-center gap-3 rounded-lg border-2 p-3 text-left transition-all disabled:cursor-not-allowed disabled:opacity-50', method === choice.value ? 'border-green-500 bg-green-50 dark:bg-green-900/20' : 'border-gray-200 hover:border-green-300 dark:border-dark-600 dark:hover:border-green-700']" @click="method = choice.value">
                    <span :class="['flex h-8 w-8 shrink-0 items-center justify-center rounded-lg', method === choice.value ? 'bg-green-500 text-white' : 'bg-gray-100 text-gray-500 dark:bg-dark-600 dark:text-gray-400']"><Icon :name="choice.icon" size="sm" /></span>
                    <span><span class="block text-sm font-medium text-gray-900 dark:text-white">{{ choice.label }}</span><span class="text-xs text-gray-500 dark:text-gray-400">{{ t(choice.hint) }}</span></span>
                  </button>
                </div><p v-if="!overview?.two_fa_login_available" class="input-hint">{{ t('hosting.twoFAUnavailable') }}</p></div>
              </template>
            </HostingAccountSettingsForm>
            <p class="input-hint">{{ t(replaceId ? 'hosting.reauthorizeConfigurationHint' : isImport ? 'hosting.importConfigurationHint' : 'hosting.addConfigurationHint') }}</p>
            <p v-if="replaceId" class="input-hint">{{ t('hosting.reauthorizePoolHint') }}</p>
          </div>
          <section v-if="addStep === 2" class="space-y-4" data-testid="hosting-add-authorization">
            <div class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800"><p class="font-medium">{{ addSettings?.name }} · {{ poolName(addSettings?.hosting_pool_id ?? 0) }}</p><p class="mt-1 text-gray-500">{{ t(isImport ? 'hosting.configurationSavedWithImport' : 'hosting.configurationSavedWithAccount') }}</p></div>
            <template v-if="method === 'oauth'"><p class="text-sm text-gray-500">{{ t('hosting.oauthHint') }}</p><button v-if="!oauth" type="button" class="btn btn-secondary min-h-11" :disabled="busy" @click="startOAuth">{{ t('hosting.startOAuth') }}</button><template v-else><a :href="oauth.auth_url" target="_blank" rel="noopener noreferrer" class="btn btn-secondary min-h-11">{{ t('hosting.openOAuth') }}</a><label for="hosting-callback" class="input-label">{{ t('hosting.callbackUrl') }}</label><textarea id="hosting-callback" v-model="callback" class="input min-h-24 w-full" :disabled="busy" autocomplete="off" /></template></template>
            <HostingTwoFALoginForm v-else-if="method === 'two_fa' && addSettings" :settings="addSettings" :replace-id="replaceId" :host="overview?.two_fa_login_host ?? ''" @busy="busy = $event" @saved="accountSaved" />
            <HostingCredentialImport v-else-if="isImport && addSettings" :settings="addSettings" :replace-id="replaceId" :available-seats="Math.max(0, (overview?.seat_limit ?? 0) - (overview?.used_seats ?? 0))" @busy="busy = $event" @imported="load" />
          </section>
          <p v-if="dialogError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ dialogError }}</p>
        </form>
        <template #footer><div class="flex flex-wrap justify-end gap-3"><button class="btn btn-secondary" :disabled="busy" @click="closeAdd">{{ t('common.cancel') }}</button><button v-if="addStep === 2" type="button" class="btn btn-secondary" :disabled="busy" @click="backAddStep">{{ t('hosting.backToConfiguration') }}</button><button v-if="addStep === 1 || method === 'oauth'" form="hosting-add-form" type="submit" class="btn btn-primary" :disabled="busy || (addStep === 2 && (!callback || !oauth))">{{ submitLabel }}</button></div></template>
      </BaseDialog>
      <BaseDialog :show="showPool" :title="t(editPoolId ? 'hosting.renamePool' : 'hosting.createPool')" @close="closePool">
        <form id="hosting-pool-form" class="space-y-4" @submit.prevent="savePool"><label class="input-label" for="hosting-pool-name">{{ t('hosting.poolName') }}</label><input id="hosting-pool-name" v-model="poolDraftName" class="input w-full" maxlength="100" required :disabled="busy" /><p v-if="poolError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ poolError }}</p></form>
        <template #footer><div class="flex justify-end gap-3"><button class="btn btn-secondary min-h-11" :disabled="busy" @click="closePool">{{ t('common.cancel') }}</button><button form="hosting-pool-form" type="submit" class="btn btn-primary min-h-11" :disabled="busy || !poolDraftName.trim()">{{ t('common.save') }}</button></div></template>
      </BaseDialog>
      <HostingImportDialog :show="showImport" :pools="overview?.pools ?? []" :initial-pool-id="Math.max(0, selectedPoolId)" :available-seats="Math.max(0, (overview?.seat_limit ?? 0) - (overview?.used_seats ?? 0))" @close="showImport = false" @imported="load" />
      <HostingAccountSettingsDialog :account="settingsTarget" :pools="overview?.pools" @close="settingsTarget = null" @saved="load" />
      <ConfirmDialog :show="!!deletePoolTarget" :title="t('hosting.deletePoolTitle')" :message="t('hosting.deletePoolHint')" danger @cancel="deletePoolTarget = null" @confirm="confirmDeletePool" />
      <ConfirmDialog :show="!!deleteTarget" :title="t('hosting.deleteTitle')" :message="t('hosting.deleteHint')" danger @cancel="deleteTarget = null" @confirm="confirmDelete" />
    </section>
  <HostingAccountStatsDialog :show="showBilling" :account="billingAccount" @close="showBilling = false" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import HostingTwoFALoginForm from '@/components/hosting/HostingTwoFALoginForm.vue'
import HostingAccountStatsDialog from '@/components/hosting/HostingAccountStatsDialog.vue'
import HostingAccountsTable from '@/components/hosting/HostingAccountsTable.vue'
import HostingAccountActions from '@/components/hosting/HostingAccountActions.vue'
import Pagination from '@/components/common/Pagination.vue'
import HostingCredentialImport from '@/components/hosting/HostingCredentialImport.vue'
import HostingImportDialog from '@/components/hosting/HostingImportDialog.vue'
import { useI18n } from 'vue-i18n'
import { formatCostFixed } from '@/utils/format'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import HostingAccountSettingsDialog from '@/components/hosting/HostingAccountSettingsDialog.vue'
import HostingAccountSettingsForm from '@/components/hosting/HostingAccountSettingsForm.vue'
import { hostingAPI, hostingErrorMessage, type HostingAccountAction, type HostedAccount, type HostedAccountSettings, type HostingOverview, type HostingPool } from '@/api/hosting'
import { useAppStore } from '@/stores/app'
const showBilling = ref(false), billingAccount = ref<HostedAccount | null>(null)
const { t } = useI18n()
const app = useAppStore()
const overview = ref<HostingOverview>()
const loading = ref(false), busy = ref(false), error = ref(''), dialogError = ref(''), showAdd = ref(false)
const loginMethods = [{ value: 'oauth', label: 'OAuth', hint: 'hosting.oauthMethodHint', icon: 'link' }, { value: 'two_fa', label: '2FA', hint: 'hosting.twoFAMethodHint', icon: 'shield' }] as const
const replaceId = ref(0), method = ref('oauth'), callback = ref('')
const addMethods = computed(() => replaceId.value ? [...loginMethods, { value: 'file', label: t('hosting.importFile'), hint: 'hosting.fileMethodHint', icon: 'document' as const }] : loginMethods)
const isImport = computed(() => method.value === 'file')
const dialogTitle = computed(() => t(replaceId.value ? 'hosting.reauthorize' : isImport.value ? 'hosting.importAccount' : 'hosting.addAccount'))
const addStep = ref(1), addSettings = ref<HostedAccountSettings>()
const submitLabel = computed(() => t(busy.value ? 'common.loading' : addStep.value === 1 ? (isImport.value ? 'hosting.nextImport' : 'hosting.nextAuthorization') : 'common.save'))
const addSettingsForm = ref<InstanceType<typeof HostingAccountSettingsForm>>()
const addInitialSettings = ref<HostedAccountSettings>({ hosting_pool_id: 0, name: 'Codex', concurrency: 3, priority: 50, credentials: {}, extra: {} })
const oauth = ref<Awaited<ReturnType<typeof hostingAPI.startOAuth>>>()
const deleteTarget = ref<HostedAccount | null>(null)
const settingsTarget = ref<HostedAccount | null>(null)
const selectedPoolId = ref(-1), editPoolId = ref(0), poolDraftName = ref(''), poolError = ref(''), showPool = ref(false)
const deletePoolTarget = ref<HostingPool | null>(null)
const selectedPool = computed(() => overview.value?.pools.find(pool => pool.id === selectedPoolId.value))
const accountSearch = ref(''), accountState = ref(''), accountPage = ref(1), accountPageSize = ref(20)
const accountStates: HostedAccount['state'][] = ['active', 'paused', 'login_required', 'quota_exhausted', 'temporarily_unavailable']
const filteredAccounts = computed(() => {
  const search = accountSearch.value.trim().toLocaleLowerCase()
  return overview.value?.accounts.filter(account =>
    (selectedPoolId.value < 0 || (account.settings.hosting_pool_id ?? 0) === selectedPoolId.value) &&
    (!accountState.value || account.state === accountState.value) &&
    (!search || [account.name, account.email, String(account.id), account.settings.notes ?? ''].some(value => value.toLocaleLowerCase().includes(search)))
  ) ?? []
})
const pagedAccounts = computed(() => filteredAccounts.value.slice((accountPage.value - 1) * accountPageSize.value, accountPage.value * accountPageSize.value))
watch([selectedPoolId, accountSearch, accountState, accountPageSize], () => { accountPage.value = 1 })
watch(() => filteredAccounts.value.length, count => { accountPage.value = Math.min(accountPage.value, Math.max(1, Math.ceil(count / accountPageSize.value))) })

const poolName = (id: number) => id === 0 ? t('hosting.defaultPool') : overview.value?.pools.find(pool => pool.id === id)?.name ?? t('hosting.pool')
const canAdd = computed(() => !!overview.value?.enabled && overview.value.used_seats < overview.value.seat_limit && !loading.value && !busy.value)
const money = (value: number) => `${formatCostFixed(value, 8)} USD`
const message = (e: unknown) => hostingErrorMessage(e, t('hosting.operationFailed'))
function updateAccount(account: HostedAccount) {
  if (!overview.value) return
  overview.value.accounts = overview.value.accounts.map(item => item.id === account.id ? account : item)
}
async function refreshUsage(account: HostedAccount) {
  if (busy.value) return
  busy.value = true
  try { const usage = await hostingAPI.accountUsage(account.id, true); updateAccount({ ...account, usage }) }
  catch (e) { app.showError(message(e)) }
  finally { busy.value = false }
}
async function load() { loading.value = true; error.value = ''; try { overview.value = await hostingAPI.overview() } catch (e) { error.value = message(e) } finally { loading.value = false } }
function openAdd(account?: HostedAccount) {
  replaceId.value = account?.id ?? 0
  addInitialSettings.value = account ? JSON.parse(JSON.stringify(account.settings)) : { hosting_pool_id: Math.max(0, selectedPoolId.value), name: 'Codex', concurrency: 3, priority: 50, auto_pause_on_expired: true, credentials: {}, extra: {} }
  callback.value = ''; oauth.value = undefined; dialogError.value = ''; method.value = 'oauth'; addStep.value = 1; addSettings.value = undefined; showAdd.value = true
}
const showImport = ref(false)
function openImport() { showImport.value = true }
function closeAdd() { if (busy.value) return; showAdd.value = false; callback.value = ''; oauth.value = undefined; addSettings.value = undefined }
async function nextAddStep() {
  dialogError.value = ''
  const settings = addSettingsForm.value?.buildSettings()
  if (!settings) return
  addSettings.value = settings; addStep.value = 2
  await nextTick(); scrollAddToTop()
}
function scrollAddToTop() { document.getElementById('hosting-add-form')?.closest('.modal-body')?.scrollTo({ top: 0 }) }
function backAddStep() { if (busy.value) return; addStep.value = 1; callback.value = ''; oauth.value = undefined; dialogError.value = ''; void nextTick(scrollAddToTop) }
async function accountSaved() { showAdd.value = false; busy.value = false; callback.value = ''; oauth.value = undefined; addSettings.value = undefined; app.showSuccess(t('hosting.saved')); await load() }
async function startOAuth() { if (!addSettings.value || busy.value) return; busy.value = true; dialogError.value = ''; try { oauth.value = await hostingAPI.startOAuth(replaceId.value, addSettings.value.hosting_pool_id ?? 0) } catch (e) { dialogError.value = message(e) } finally { busy.value = false } }
async function submit() {
  if (busy.value || !addSettings.value || addStep.value !== 2 || method.value !== 'oauth' || !oauth.value || !callback.value.trim()) return
  busy.value = true; dialogError.value = ''
  try {
    const settings = addSettings.value
    await hostingAPI.completeOAuth(oauth.value.session_id, callback.value, settings.name, settings)
    await accountSaved()
  } catch (e) { dialogError.value = message(e) }
  finally { busy.value = false }
}
async function change(account: HostedAccount, action: HostingAccountAction) { if (busy.value) return; busy.value = true; try { await hostingAPI.change(account.id, action); deleteTarget.value = null; await load() } catch (e) { app.showError(message(e)) } finally { busy.value = false } }
async function confirmDelete() { if (deleteTarget.value) await change(deleteTarget.value, 'delete') }
function openPool(pool?: HostingPool) { editPoolId.value = pool?.id ?? 0; poolDraftName.value = pool?.name ?? ''; poolError.value = ''; showPool.value = true }
function closePool() { if (!busy.value) showPool.value = false }
async function savePool() { if (busy.value) return; busy.value = true; poolError.value = ''; try { const pool = await hostingAPI.savePool(poolDraftName.value.trim(), editPoolId.value); selectedPoolId.value = pool.id; showPool.value = false; app.showSuccess(t('hosting.saved')); await load() } catch (e) { poolError.value = message(e) } finally { busy.value = false } }
async function confirmDeletePool() { if (busy.value || !deletePoolTarget.value) return; busy.value = true; try { await hostingAPI.deletePool(deletePoolTarget.value.id); deletePoolTarget.value = null; selectedPoolId.value = -1; await load() } catch (e) { app.showError(message(e)) } finally { busy.value = false } }
onMounted(load)
</script>
