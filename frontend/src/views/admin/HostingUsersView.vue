<template>
  <AppLayout>
    <TablePageLayout>
      <template #actions><div class="space-y-4"><header><h1 class="text-2xl font-semibold">{{ t('hosting.adminUsersNav') }}</h1><p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('hosting.adminUsersDescription') }}</p></header><HostingAdminNav /></div></template>
      <template #filters>
        <form id="hosting-user-search-form" class="flex flex-wrap items-end gap-3" @submit.prevent="searchUsers">
          <div class="min-w-0 flex-1 sm:min-w-64"><label for="hosting-user-search" class="input-label">{{ t('hosting.searchUser') }}</label><input id="hosting-user-search" v-model="searchDraft" class="input w-full" maxlength="200" :placeholder="t('hosting.searchUser')" /></div>
          <button class="btn btn-primary min-h-11" :disabled="loading">{{ t('common.search') }}</button><button type="button" class="btn btn-secondary min-h-11" :disabled="loading" @click="load(page)">{{ t('common.refresh') }}</button>
        </form>
        <p v-if="error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      </template>
      <template #table>
        <div class="table-wrapper" :aria-busy="loading"><table class="w-full text-left text-sm"><thead class="sticky top-0 z-10"><tr><th>{{ t('hosting.ownerUser') }}</th><th>{{ t('common.status') }}</th><th>{{ t('common.actions') }}</th></tr></thead>
          <tbody><tr v-if="loading"><td colspan="3" class="py-12 text-center text-gray-500" role="status">{{ t('common.loading') }}</td></tr><tr v-else-if="!users.length"><td colspan="3" class="py-12 text-center text-gray-500">{{ t('hosting.adminUsersEmpty') }}</td></tr>
            <tr v-for="user in loading ? [] : users" :key="user.id" class="hover:bg-gray-50 dark:hover:bg-dark-800/50"><td><p class="font-medium">{{ user.username || user.email }}</p><p class="mt-1 text-xs text-gray-500">{{ user.email }} · #{{ user.id }}</p></td><td>{{ t(user.status === 'active' ? 'hosting.userActive' : 'hosting.userDisabled') }}</td><td><div class="flex flex-wrap gap-2"><button class="btn btn-secondary" @click="policyUser = user">{{ t('hosting.editUserPolicy') }}</button><RouterLink class="btn btn-secondary" :to="{ path: '/admin/hosting/accounts', query: { user_id: user.id } }">{{ t('hosting.viewUserAccounts') }}</RouterLink></div></td></tr>
          </tbody>
        </table></div>
      </template>
      <template #pagination><Pagination v-if="total > 0" :page="page" :page-size="20" :show-page-size-selector="false" show-jump :total="total" @update:page="load" /></template>
    </TablePageLayout>
    <HostingUserPolicyDialog :user="policyUser" @close="policyUser = null" />
  </AppLayout>
</template>
<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import HostingAdminNav from '@/components/admin/hosting/HostingAdminNav.vue'
import HostingUserPolicyDialog from '@/components/admin/hosting/HostingUserPolicyDialog.vue'
import { usersAPI } from '@/api/admin/users'
import { hostingErrorMessage } from '@/api/hosting'
import type { AdminUser } from '@/types'
const { t } = useI18n()
const users = ref<AdminUser[]>([]), searchDraft = ref(''), appliedSearch = ref('')
const page = ref(1), total = ref(0), loading = ref(false), error = ref('')
const policyUser = ref<AdminUser | null>(null)
let request = 0
async function load(targetPage = 1) {
  const current = ++request
  loading.value = true; error.value = ''
  try {
    const result = await usersAPI.list(targetPage, 20, { search: appliedSearch.value })
    if (current !== request) return
    const lastPage = Math.max(1, Math.ceil(result.total / 20))
    if (targetPage > lastPage) { await load(lastPage); return }
    users.value = result.items; total.value = result.total; page.value = targetPage
  } catch (e) { if (current === request) error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { if (current === request) loading.value = false }
}
function searchUsers() { appliedSearch.value = searchDraft.value.trim(); void load(1) }
onMounted(() => load())
</script>
