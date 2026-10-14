<template>
  <div class="space-y-4">
    <p class="input-hint">{{ t('hosting.twoFAHint', { host }) }}</p>
    <template v-if="!job">
      <div><label for="hosting-login-email" class="input-label">{{ t('hosting.loginEmail') }}</label><input id="hosting-login-email" v-model="email" type="email" class="input" autocomplete="off" :disabled="busy" /></div>
      <div><label for="hosting-login-password" class="input-label">{{ t('hosting.loginPassword') }}</label><input id="hosting-login-password" v-model="password" type="password" class="input" autocomplete="new-password" :disabled="busy" /></div>
      <div><label for="hosting-login-mfa" class="input-label">{{ t('hosting.loginMFA') }}</label><input id="hosting-login-mfa" v-model="mfa" type="password" class="input font-mono" autocomplete="off" :spellcheck="false" :disabled="busy" /><p class="input-hint">{{ t('hosting.loginMFAHint') }}</p></div>
    </template>
    <p v-if="busy" role="status" class="flex items-center gap-2 text-sm text-gray-500"><Icon name="refresh" size="sm" class="animate-spin" />{{ t('hosting.twoFALoggingIn') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <div class="flex gap-3">
      <button v-if="!busy" type="button" class="btn btn-primary" :disabled="!job && (!email.trim() || !password || !mfa.trim())" @click="run">{{ t(job ? 'hosting.twoFARetrySave' : 'hosting.twoFAStart') }}</button>
      <button v-if="busy || job" type="button" class="btn btn-secondary" :disabled="saving" @click="cancel">{{ t('hosting.twoFACancel') }}</button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { hostingAPI, hostingErrorMessage, type HostedAccountSettings, type HostingTwoFAJob } from '@/api/hosting'
const props = defineProps<{ settings: HostedAccountSettings; replaceId: number; host: string }>()
const emit = defineEmits<{ busy: [value: boolean]; saved: [] }>()
const { t } = useI18n()
const email = ref(''), password = ref(''), mfa = ref(''), error = ref(''), busy = ref(false), saving = ref(false)
const job = ref<HostingTwoFAJob>()
let generation = 0, disposed = false
let pollTimer: ReturnType<typeof setTimeout> | undefined, releasePoll: (() => void) | undefined
function clearInputs() { email.value = ''; password.value = ''; mfa.value = '' }
async function discard() { const id = job.value?.id; job.value = undefined; if (id) await hostingAPI.deleteTwoFALogin(id).catch(() => {}) }
async function cancel() { generation++; clearTimeout(pollTimer); releasePoll?.(); releasePoll = undefined; clearInputs(); await discard(); busy.value = false; emit('busy', false) }
async function run() {
  if (busy.value || disposed) return
  const current = ++generation
  busy.value = true; emit('busy', true); error.value = ''
  try {
    if (!job.value) {
      const started = await hostingAPI.startTwoFALogin({ email: email.value.trim(), password: password.value, mfa_secret: mfa.value.trim() }, props.replaceId, props.settings.hosting_pool_id ?? 0)
      if (current !== generation || disposed) { await hostingAPI.deleteTwoFALogin(started.id).catch(() => {}); return }
      job.value = started; clearInputs()
    }
    while (job.value?.status === 'running') {
      await new Promise<void>(resolve => { releasePoll = resolve; pollTimer = setTimeout(resolve, 1500) })
      releasePoll = undefined
      if (current !== generation || disposed || !job.value) return
      const status = await hostingAPI.getTwoFALogin(job.value.id)
      if (current !== generation || disposed) return
      job.value = status
    }
    if (current !== generation || disposed || !job.value) return
    if (job.value.status !== 'succeeded') { await discard(); throw new Error(t('hosting.twoFAFailed')) }
    saving.value = true
    await hostingAPI.completeTwoFALogin(job.value.id, props.settings)
    job.value = undefined
    if (current === generation && !disposed) emit('saved')
  } catch (cause) {
    if (current === generation && !disposed) {
      error.value = hostingErrorMessage(cause, t('hosting.operationFailed'))
      const status = (cause as { status?: number; response?: { status?: number } })?.status ?? (cause as { response?: { status?: number } })?.response?.status
      if (status === 404) job.value = undefined
    }
  } finally { if (current === generation) { busy.value = false; saving.value = false; emit('busy', false) } }
}
onBeforeUnmount(() => { disposed = true; void cancel() })
</script>
