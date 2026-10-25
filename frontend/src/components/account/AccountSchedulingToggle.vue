<template>
  <button
    type="button"
    role="switch"
    :aria-checked="modelValue === true"
    :aria-label="`${accountName} · ${t('admin.accounts.columns.schedulable')}`"
    :disabled="disabled || modelValue == null"
    class="relative inline-flex h-5 w-9 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 dark:focus:ring-offset-dark-800"
    :class="modelValue ? 'bg-primary-500 hover:bg-primary-600' : 'bg-gray-200 hover:bg-gray-300 dark:bg-dark-600 dark:hover:bg-dark-500'"
    :title="t(modelValue ? 'admin.accounts.schedulableEnabled' : 'admin.accounts.schedulableDisabled')"
    @click="emit('update:modelValue', !modelValue)"
  >
    <span class="pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out" :class="modelValue ? 'translate-x-4' : 'translate-x-0'" />
  </button>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
defineProps<{ modelValue?: boolean | null; accountName: string; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()
const { t } = useI18n()
</script>
