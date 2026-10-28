<template>
  <span data-testid="docker-system-binding" class="text-xs text-gray-500 dark:text-gray-400">
    {{ t('admin.accounts.dockerSystem.bound') }}: {{ profile ? dockerSystemProfileName(profile) : t('admin.accounts.dockerSystem.legacy') }}
  </span>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { dockerSystemProfileName } from '@/utils/dockerSystemProfile'
defineProps<{ profile?: string | null }>()
const { t } = useI18n()
</script>
