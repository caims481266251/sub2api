<script setup lang="ts">
import { useI18n } from 'vue-i18n'
const model = defineModel<boolean>({ required: true })
const { t } = useI18n()
</script>

<template>
  <div class="rounded-xl border border-gray-200 p-4 dark:border-dark-600" data-testid="docker-request-cache">
    <label class="flex items-center gap-2 text-sm font-medium text-gray-800 dark:text-gray-100">
      <input v-model="model" type="checkbox" class="rounded" data-testid="docker-request-cache-enabled" />
      {{ t('admin.accounts.dockerRequestCache.title') }}
    </label>
    <p class="input-hint mt-2">{{ t('admin.accounts.dockerRequestCache.hint') }}</p>
  </div>
</template>
