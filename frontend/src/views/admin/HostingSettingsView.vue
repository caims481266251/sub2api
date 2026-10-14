<template>
  <AppLayout>
    <section class="space-y-5">
      <header><h1 class="text-2xl font-semibold">{{ t('hosting.adminSettingsNav') }}</h1><p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('hosting.adminDescription') }}</p></header>
      <HostingAdminNav />
      <p v-if="error" role="alert" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300">{{ error }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <button v-else-if="!settings" class="btn btn-secondary" @click="load">{{ t('common.refresh') }}</button>
      <form v-if="settings" class="card max-w-4xl space-y-5 p-6" @submit.prevent="saveSettings">
        <h2 class="text-lg font-semibold">{{ t('hosting.globalSettings') }}</h2>
        <fieldset :disabled="busy" class="min-w-0 space-y-5">
          <label class="flex min-h-11 items-center gap-3"><input v-model="settings.enabled" type="checkbox" role="switch" />{{ t('hosting.featureEnabled') }}</label>
          <div class="grid gap-5 sm:grid-cols-2">
            <div><label for="hosting-default-seats" class="input-label">{{ t('hosting.defaultSeats') }}</label><input id="hosting-default-seats" v-model.number="settings.default_seats" class="input w-full" type="number" min="0" max="1000" step="1" required /><p class="mt-2 text-xs text-gray-500">{{ t('hosting.seatsHint') }}</p></div>
            <div><label for="hosting-default-rate" class="input-label">{{ t('hosting.defaultRate') }}</label><input id="hosting-default-rate" v-model.number="settings.rate_multiplier" class="input w-full" type="number" min="0" max="999999.9999" step="0.0001" required /><p class="mt-2 text-sm text-teal-700 dark:text-teal-300">100 × {{ settings.rate_multiplier }} = {{ (100 * settings.rate_multiplier).toFixed(4) }} USD</p></div>
          </div>
        </fieldset>
        <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t('hosting.billingExplanation') }}</p>
        <div class="flex justify-end"><button class="btn btn-primary min-h-11" :disabled="busy">{{ t(busy ? 'common.loading' : 'common.save') }}</button></div>
      </form>
    </section>
  </AppLayout>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import HostingAdminNav from '@/components/admin/hosting/HostingAdminNav.vue'
import { hostingAPI, hostingErrorMessage, type HostingSettings } from '@/api/hosting'
import { useAppStore } from '@/stores/app'
const { t } = useI18n(), app = useAppStore()
const settings = ref<HostingSettings>(), loading = ref(false), busy = ref(false), error = ref('')
async function load() {
  loading.value = true; error.value = ''
  try { settings.value = await hostingAPI.settings() }
  catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { loading.value = false }
}
async function saveSettings() {
  if (!settings.value || busy.value) return
  busy.value = true; error.value = ''
  try { await hostingAPI.saveSettings({ ...settings.value }); app.showSuccess(t('hosting.saved')) }
  catch (e) { error.value = hostingErrorMessage(e, t('hosting.operationFailed')) }
  finally { busy.value = false }
}
onMounted(load)
</script>
