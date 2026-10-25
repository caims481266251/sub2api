<template>
  <div class="flex items-center gap-1 whitespace-nowrap">
    <button :disabled="disabled || operating || configurationDisabled" :title="t('hosting.accountSettings')" class="flex min-h-11 min-w-11 flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 disabled:opacity-50 dark:hover:bg-dark-700 dark:hover:text-primary-400" @click="emit('edit')"><Icon name="edit" size="sm" /><span class="text-xs">{{ t('common.edit') }}</span></button>
    <button :disabled="disabled || operating" class="flex min-h-11 min-w-11 flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 disabled:opacity-50 dark:hover:bg-red-900/20 dark:hover:text-red-400" @click="emit('delete')"><Icon name="trash" size="sm" /><span class="text-xs">{{ t('common.delete') }}</span></button>
    <button ref="trigger" :disabled="disabled || operating" :aria-expanded="show" aria-haspopup="true" class="flex min-h-11 min-w-11 flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-900 disabled:opacity-50 dark:hover:bg-dark-700 dark:hover:text-white" @click="open"><Icon name="more" size="sm" /><span class="text-xs">{{ t('common.more') }}</span></button>
    <AccountActionMenu :show="show" :account="null" :hosting-account="account" :anchor-rect="anchor" :disabled="disabled || operating" :allow-hosting-reauth="!admin && !configurationDisabled" :show-hosting-policy="admin" :allow-hosting-i-q-test="admin" @iq-test="admin && (showIQTest = true)" @test="showTest = true" @refresh-token="operate('refresh')" @set-privacy="operate('set-privacy')" @recover-state="showRecovery = true" @stats="emit('stats')" @close="close" @reauth="emit('reauth')" @refresh-usage="emit('refreshUsage')" @toggle-hosting="emit('toggle')" @hosting-policy="emit('policy')" />
    <AccountTestModal v-if="showTest" :show="showTest" :account="view" :load-models="loadModels" :test-path="testPath" diagnostic-only @close="showTest = false; emit('reload')" />
    <IQTestModal v-if="admin && showIQTest" :show="showIQTest" :account="view" @close="showIQTest = false; emit('reload')" />
    <ConfirmDialog :show="showRecovery" :title="t('admin.accounts.recoverState')" :message="t('admin.accounts.recoverStateHint')" @cancel="showRecovery = false" @confirm="showRecovery = false; operate('recover-state')" />
  </div>
</template>
<script setup lang="ts">
import { computed, ref, onBeforeUnmount, watch, defineAsyncComponent } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'
import { hostingAPI, hostingErrorMessage, type HostedAccount } from '@/api/hosting'
import AccountTestModal from '@/components/admin/account/AccountTestModal.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { useAppStore } from '@/stores/app'
import { hostedAccountView } from './hostedAccountView'
const IQTestModal = defineAsyncComponent(() => import('@/components/admin/account/IQTestModal.vue'))
const showIQTest = ref(false)
const props = defineProps<{ account: HostedAccount; disabled?: boolean; configurationDisabled?: boolean; admin?: boolean }>()
const emit = defineEmits<{ updated: [account: HostedAccount]; reload: []; stats: []; edit: []; delete: []; reauth: []; refreshUsage: []; toggle: []; policy: [] }>()
const { t } = useI18n()
const app = useAppStore()
const operating = ref(false), showTest = ref(false), showRecovery = ref(false)
const view = computed(() => hostedAccountView(props.account))
const testPath = computed(() => `${props.admin ? '/admin' : ''}/hosting/accounts/${props.account.id}/operations/test`)
const loadModels = (id: number) => hostingAPI.models(id, props.admin)
async function operate(operation: 'refresh' | 'recover-state' | 'set-privacy') {
  if (operating.value || props.disabled) return
  operating.value = true
  try {
    const updated = await hostingAPI.operation(props.account.id, operation, props.admin)
    emit('updated', updated)
    if (operation === 'set-privacy') {
      const mode = updated.runtime_extra?.privacy_mode
      if (mode === 'training_off') app.showSuccess(t('admin.accounts.privacyTrainingOff'))
      else app.showError(t(mode === 'training_set_cf_blocked' ? 'admin.accounts.privacyCfBlocked' : 'admin.accounts.privacyFailed'))
    } else app.showSuccess(t(operation === 'recover-state' ? 'admin.accounts.recoverStateSuccess' : 'hosting.saved'))
  }
  catch (e) { app.showError(hostingErrorMessage(e, t('hosting.operationFailed'))) }
  finally { operating.value = false }
}
const trigger = ref<HTMLButtonElement>(), anchor = ref<DOMRect | null>(null), show = ref(false)
function close() { show.value = false }
function closeOnScroll(event: Event) {
  if (event.target instanceof Element && event.target.closest('.action-menu-content')) return
  close()
}
function open() { anchor.value = trigger.value?.getBoundingClientRect() ?? null; show.value = !show.value }
watch(show, visible => {
  if (visible) window.addEventListener('scroll', closeOnScroll, true)
  else window.removeEventListener('scroll', closeOnScroll, true)
})
watch(() => props.disabled, disabled => { if (disabled) close() })
onBeforeUnmount(() => window.removeEventListener('scroll', closeOnScroll, true))
</script>
