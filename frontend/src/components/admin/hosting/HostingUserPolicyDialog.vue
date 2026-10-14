<template>
  <BaseDialog :show="!!user" :title="t('hosting.userPolicy')" @close="close">
    <template v-if="user">
      <div class="mb-5 rounded-xl bg-gray-50 p-4 dark:bg-dark-800"><p class="font-medium">{{ user.username || user.email }}</p><p class="mt-1 break-all text-sm text-gray-500">{{ user.email }} · #{{ user.id }}</p></div>
      <p v-if="error" role="alert" class="mb-4 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <button v-else-if="!loaded" class="btn btn-secondary" @click="load">{{ t('common.refresh') }}</button>
      <form v-if="loaded" id="hosting-user-policy-form" class="space-y-5" @submit.prevent="save">
        <p class="text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('hosting.overrideHint') }}</p>
        <fieldset :disabled="saving" class="grid min-w-0 gap-5 sm:grid-cols-2">
          <div><label for="hosting-user-seats" class="input-label">{{ t('hosting.seatOverride') }}</label><input id="hosting-user-seats" v-model="seatOverride" class="input w-full" type="number" min="0" max="1000" step="1" :placeholder="t('hosting.inherit')" /></div>
          <div><label for="hosting-user-rate" class="input-label">{{ t('hosting.rateOverride') }}</label><input id="hosting-user-rate" v-model="rateOverride" class="input w-full" type="number" min="0" max="999999.9999" step="0.0001" :placeholder="t('hosting.inherit')" /></div>
        </fieldset>
        <p class="text-sm text-gray-500">{{ t('hosting.effectivePolicy', { seats: effectiveSeats, rate: effectiveRate }) }}</p>
      </form>
    </template>
    <template #footer><div class="flex justify-end gap-3"><button class="btn btn-secondary min-h-11" :disabled="saving" @click="close">{{ t('common.cancel') }}</button><button v-if="loaded" form="hosting-user-policy-form" type="submit" class="btn btn-primary min-h-11" :disabled="saving">{{ t(saving ? 'common.loading' : 'common.save') }}</button></div></template>
  </BaseDialog>
</template>
<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { hostingAPI, hostingErrorMessage } from '@/api/hosting'
import { useAppStore } from '@/stores/app'
const props = defineProps<{ user: { id: number; username: string; email: string } | null }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n(), app = useAppStore()
const seatOverride = ref<string | number>(''), rateOverride = ref<string | number>('')
const effectiveSeats = ref(1), effectiveRate = ref(.03)
const loading = ref(false), saving = ref(false), loaded = ref(false), error = ref('')
let request = 0
async function load() {
  const user = props.user, current = ++request
  loaded.value = false; error.value = ''
  if (!user) { loading.value = false; return }
  loading.value = true
  try {
    const result = await hostingAPI.policy(user.id)
    if (current !== request) return
    seatOverride.value = result.policy.seat_limit ?? ''; rateOverride.value = result.policy.rate_multiplier ?? ''
    effectiveSeats.value = result.seat_limit; effectiveRate.value = result.rate_multiplier; loaded.value = true
  } catch (e) { if (current === request) error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { if (current === request) loading.value = false }
}
function close() { if (!saving.value) emit('close') }
async function save() {
  const user = props.user
  if (!user || !loaded.value || saving.value) return
  saving.value = true; error.value = ''
  try {
    await hostingAPI.savePolicy(user.id, { seat_limit: seatOverride.value === '' ? null : Number(seatOverride.value), rate_multiplier: rateOverride.value === '' ? null : Number(rateOverride.value) })
    app.showSuccess(t('hosting.saved')); emit('saved'); emit('close')
  } catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { saving.value = false }
}
watch(() => props.user?.id, load, { immediate: true })
</script>
