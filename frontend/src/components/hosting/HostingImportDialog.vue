<template>
  <BaseDialog :show="show" :title="t('hosting.importAccount')" width="wide" close-on-click-outside @close="close">
    <form v-if="show" id="hosting-import-form" class="space-y-4" novalidate @submit.prevent="submit">
      <HostingCredentialImport ref="importer" :settings="settings" :available-seats="availableSeats" hide-actions @busy="busy = $event" @imported="emit('imported')" />
      <fieldset :disabled="busy || importer?.hasResults" class="min-w-0 space-y-4">
        <div>
          <label for="hosting-import-pool" class="input-label">{{ t('hosting.pool') }}</label>
          <select id="hosting-import-pool" v-model.number="poolId" class="input">
            <option v-for="pool in pools" :key="pool.id" :value="pool.id">{{ pool.id === 0 ? t('hosting.defaultPool') : pool.name }}</option>
          </select>
        </div>
      </fieldset>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button class="btn btn-secondary" type="button" :disabled="busy" @click="close">{{ t('common.cancel') }}</button>
        <button v-if="importer?.canRetryOrSubmit ?? true" class="btn btn-primary" type="submit" form="hosting-import-form" data-testid="hosting-import-submit" :disabled="!importer?.canSubmit">{{ importer?.submitLabel ?? t('admin.accounts.dataImportButton') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import HostingCredentialImport from './HostingCredentialImport.vue'
import type { HostedAccountSettings, HostingPool } from '@/api/hosting'

const props = defineProps<{ show: boolean; pools: HostingPool[]; initialPoolId: number; availableSeats: number }>()
const emit = defineEmits<{ close: []; imported: [] }>()
const { t } = useI18n()
const defaults = (): HostedAccountSettings => ({ name: 'Codex', hosting_pool_id: props.initialPoolId, concurrency: 3, priority: 50, auto_pause_on_expired: true, credentials: {}, extra: {} })
const settings = ref(defaults()), poolId = ref(props.initialPoolId), busy = ref(false)
const importer = ref<InstanceType<typeof HostingCredentialImport>>()
watch(() => props.show, show => { if (show) { settings.value = defaults(); poolId.value = props.initialPoolId } })
function close() { if (!busy.value) emit('close') }
async function submit() {
  if (busy.value || !importer.value?.canSubmit) return
  if (!importer.value.hasResults) {
    settings.value = { ...defaults(), hosting_pool_id: poolId.value }
    await nextTick()
  }
  await importer.value.startImport()
}
</script>
