<template>
  <section class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600" data-testid="saved-two-fa">
    <p v-if="loading" class="text-sm">{{ t('savedTwoFA.loading') }}</p>
    <template v-else>
      <p class="text-sm">{{ configured ? t('savedTwoFA.saved') : t('savedTwoFA.missing') }}</p>
      <a href="/admin/token-guard-v2" target="_blank" rel="noopener noreferrer" class="text-sm text-primary-600">{{ t('savedTwoFA.manage') }}</a>
      <p v-if="task" class="text-sm" role="status">{{ t('savedTwoFA.states.' + task.status) }}</p>
      <p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-primary" data-testid="saved-two-fa-reauth" :disabled="!configured || pending || submitting" @click="reauthorize">{{ t('savedTwoFA.reauthorize') }}</button>
        <button type="button" class="btn btn-secondary" data-testid="saved-two-fa-copy" :disabled="!configured || copying" @click="copy">{{ t('savedTwoFA.copy') }}</button>
        <button type="button" class="btn btn-secondary" @click="refresh">{{ t('savedTwoFA.refresh') }}</button>
      </div>
      <p v-if="copied" role="status" class="text-sm text-green-600">{{ t('savedTwoFA.copied') }}</p>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'
import { exportTOTP } from '@/api/admin/openaiTotp'
import { useClipboard } from '@/composables/useClipboard'
import type { TokenGuardV2LoginConfig, TokenGuardV2Task } from '@/api/admin/accountTokenGuardV2'
const props = defineProps<{ accountId: number }>()
const emit = defineEmits<{ reauthorized: [] }>()
const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const loading = ref(true)
const configured = ref(false)
const task = ref<TokenGuardV2Task | null>(null)
const submitting = ref(false)
const copying = ref(false)
const copied = ref(false)
const error = ref('')
const pending = computed(() => !!task.value && ['queued', 'running', 'callback_processing'].includes(task.value.status))
let generation = 0
let timer: ReturnType<typeof setTimeout> | undefined
let controller = new AbortController()
let completedTask: number | undefined
let observedPendingTask: number | undefined

async function refresh() {
  const current = generation
  if (timer) clearTimeout(timer)
  try {
    const { data } = await apiClient.get<{ config?: TokenGuardV2LoginConfig; task?: TokenGuardV2Task }>('/admin/accounts/' + props.accountId + '/openai-reauth', { signal: controller.signal })
    if (current !== generation) return
    configured.value = !!(data.config?.configured && data.config.credential_mode === 'password_totp' && data.config.totp_configured)
    task.value = data.task ?? null
    error.value = ''
    if (pending.value) observedPendingTask = task.value?.id
    if (task.value?.status === 'succeeded' && observedPendingTask === task.value.id && completedTask !== task.value.id) {
      completedTask = task.value.id
      emit('reauthorized')
    }
    if (pending.value) timer = setTimeout(refresh, 1500)
  } catch {
    if (current === generation) error.value = t('savedTwoFA.loadFailed')
  } finally {
    if (current === generation) loading.value = false
  }
}
async function reauthorize() {
  if (submitting.value || pending.value || !configured.value) return
  const current = generation
  submitting.value = true
  error.value = ''
  try {
    const { data } = await apiClient.post<TokenGuardV2Task>('/admin/accounts/' + props.accountId + '/openai-reauth', { oauth_profile: 'codex' }, { signal: controller.signal })
    if (current !== generation) return
    task.value = data
    observedPendingTask = data.id
    await refresh()
  } catch {
    if (current === generation) error.value = t('savedTwoFA.startFailed')
  } finally {
    if (current === generation) submitting.value = false
  }
}
async function copy() {
  if (copying.value || !configured.value) return
  const current = generation
  copying.value = true
  copied.value = false
  error.value = ''
  let plaintext = ''
  let credential: Awaited<ReturnType<typeof exportTOTP>> | undefined
  try {
    credential = await exportTOTP(props.accountId, 'current', true, controller.signal)
    if (current !== generation) return
    if (!credential.password || !credential.secret) throw new Error('incomplete')
    plaintext = credential.email + '----' + credential.password + '----' + credential.secret
    if (!await copyToClipboard(plaintext, t('savedTwoFA.copied'))) throw new Error('copy_failed')
    if (current === generation) copied.value = true
  } catch {
    if (current === generation) error.value = t('savedTwoFA.copyFailed')
  } finally {
    plaintext = ''
    if (credential) { credential.password = ''; credential.secret = ''; credential.otpauth_uri = ''; credential.email = '' }
    if (current === generation) copying.value = false
  }
}
watch(() => props.accountId, () => {
  generation++
  controller.abort()
  controller = new AbortController()
  if (timer) clearTimeout(timer)
  task.value = null
  configured.value = false
  copied.value = false
  copying.value = submitting.value = false
  loading.value = true
  completedTask = undefined
  observedPendingTask = undefined
  void refresh()
}, { immediate: true })
onBeforeUnmount(() => { generation++; controller.abort(); if (timer) clearTimeout(timer) })
</script>
