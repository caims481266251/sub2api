<template>
  <fieldset data-testid="docker-system-selector" class="rounded-lg border border-gray-200 p-4 dark:border-dark-600" :aria-busy="loading">
    <legend class="px-1 text-sm font-medium">{{ t('admin.accounts.dockerSystem.title') }}</legend>
    <div class="mb-3 flex items-start justify-between gap-3">
      <p class="input-hint">{{ t('admin.accounts.dockerSystem.hint') }}</p>
      <button type="button" class="btn btn-secondary shrink-0 text-xs" data-testid="docker-system-refresh" :disabled="loading || locked" @click="$emit('refresh')">
        {{ t('common.refresh') }}
      </button>
    </div>
    <p v-if="loading" role="status" class="mb-3 text-sm text-gray-500">{{ t('admin.accounts.dockerSystem.loading') }}</p>
    <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ t('admin.accounts.dockerSystem.loadFailed') }}</p>
    <div class="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
      <label v-for="option in options" :key="option.profile_id"
        :class="['flex gap-2 rounded-lg border p-3 text-sm', modelValue === option.profile_id ? 'border-primary-500 bg-primary-50 dark:bg-primary-950/30' : 'border-gray-200 dark:border-dark-600', !option.available || loading || error || locked ? 'cursor-not-allowed opacity-60' : 'cursor-pointer']">
        <input type="radio" name="docker-system-profile" :value="option.profile_id" :checked="modelValue === option.profile_id"
          :data-testid="'docker-system-' + option.profile_id" :disabled="!option.available || loading || error || locked"
          class="mt-0.5 text-primary-600" @change="$emit('update:modelValue', option.profile_id)" />
        <span class="min-w-0">
          <span class="block font-medium">{{ option.display_name }}</span>
          <span v-if="option.profile_id === 'auto'" class="mt-1 block text-xs text-gray-500">{{ t('admin.accounts.dockerSystem.autoHint') }}</span>
          <span v-else class="mt-1 block text-xs text-gray-500">
            {{ error ? t('admin.accounts.dockerSystem.countUnavailable') : option.bound_account_count === null ? t('admin.accounts.dockerSystem.countPending') : t('admin.accounts.dockerSystem.boundCount', { count: option.bound_account_count }) }}
          </span>
          <span v-if="!option.available && !loading && !error" class="mt-1 block break-words text-xs text-amber-700 dark:text-amber-400">
            {{ unavailableReason(option.unavailable_reason) }}
          </span>
        </span>
      </label>
    </div>
    <p v-if="pool?.count_scope === 'visible'" class="mt-3 text-xs text-gray-500">{{ t('admin.accounts.dockerSystem.visibleCounts') }}</p>
    <p v-if="pool && !error" class="mt-3 text-xs text-gray-500">{{ t('admin.accounts.dockerSystem.legacyCount', { count: pool.legacy_bound_account_count }) }}</p>
    <p v-if="pool && !error" class="mt-1 text-xs text-gray-500">{{ t('admin.accounts.dockerSystem.checkedAt', { time: formatDateTime(pool.checked_at) }) }}</p>
  </fieldset>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DockerSystemPool } from '@/types'
import { formatDateTime } from '@/utils/format'
import type { DockerSystemSelection } from '@/utils/dockerSystemProfile'
const props = defineProps<{
  modelValue: DockerSystemSelection
  profiles: Array<{ profile_id: string; display_name: string; available: boolean; unavailable_reason?: string | null; bound_account_count: number | null }>
  pool: DockerSystemPool | null
  loading: boolean
  error: boolean
  locked?: boolean
}>()
defineEmits<{ 'update:modelValue': [value: DockerSystemSelection]; refresh: [] }>()
const { t } = useI18n()
function unavailableReason(reason?: string | null): string {
  switch (reason) {
    case 'architecture_not_supported': return t('admin.accounts.dockerSystem.reasonArchitecture')
    case 'profile_not_installed':
    case 'template_not_installed': return t('admin.accounts.dockerSystem.reasonNotInstalled')
    case 'profile_image_missing': return t('admin.accounts.dockerSystem.reasonImageMissing')
    case 'profile_validation_failed': return t('admin.accounts.dockerSystem.reasonValidationFailed')
    case 'runtime_catalog_unverified': return t('admin.accounts.dockerSystem.reasonCatalogUnverified')
    case 'runtime_host_unavailable': return t('admin.accounts.dockerSystem.reasonHostUnavailable')
    default: return t('admin.accounts.dockerSystem.unavailable')
  }
}
const options = computed(() => [{ profile_id: 'auto' as const, display_name: t('admin.accounts.dockerSystem.auto'),
  available: props.profiles.some(profile => profile.available), bound_account_count: null, unavailable_reason: '' }, ...props.profiles.map(profile => ({ ...profile, profile_id: profile.profile_id as DockerSystemSelection }))])
</script>
