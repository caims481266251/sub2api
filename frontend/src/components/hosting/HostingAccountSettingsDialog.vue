<template>
  <BaseDialog :show="!!account" :title="t('hosting.accountSettings')" width="wide" @close="close">
    <form v-if="account" id="hosting-settings-form" class="space-y-5" @submit.prevent="save">
      <HostingAccountSettingsForm ref="settingsForm" :settings="account.settings" :account-id="account.id" :admin="admin" :pools="pools" :disabled="busy" />
      <p class="input-hint">{{ t('hosting.accountSettingsHint') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3"><button class="btn btn-secondary" :disabled="busy" @click="close">{{ t('common.cancel') }}</button><button type="submit" form="hosting-settings-form" class="btn btn-primary" :disabled="busy">{{ t(busy ? 'common.loading' : 'common.save') }}</button></div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import HostingAccountSettingsForm from './HostingAccountSettingsForm.vue'
import { hostingAPI, hostingErrorMessage, type HostedAccount, type HostingPool } from '@/api/hosting'
import { useAppStore } from '@/stores/app'
const props = defineProps<{ account: HostedAccount | null; pools?: HostingPool[]; admin?: boolean }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n(), app = useAppStore()
const settingsForm = ref<InstanceType<typeof HostingAccountSettingsForm>>()
const busy = ref(false), error = ref('')
watch(() => props.account, () => { error.value = '' })
function close() { if (!busy.value) emit('close') }
async function save() {
  if (!props.account || busy.value) return
  const settings = settingsForm.value?.buildSettings()
  if (!settings) return
  busy.value = true; error.value = ''
  try {
    if (props.admin) await hostingAPI.saveAccountSettings(props.account.id, settings, true)
    else await hostingAPI.saveAccountSettings(props.account.id, settings)
    app.showSuccess(t('hosting.saved')); emit('saved'); emit('close')
  } catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { busy.value = false }
}
</script>
