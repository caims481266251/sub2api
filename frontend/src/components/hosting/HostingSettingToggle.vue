<template>
  <div class="flex items-center justify-between gap-4">
    <div class="min-w-0 flex-1">
      <label class="input-label mb-0">{{ label }}</label>
      <p v-if="hint" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ hint }}</p>
    </div>
    <Toggle :model-value="modelValue" :disabled="disabled" :aria-label="label" :data-testid="testId" @update:model-value="$emit('update:modelValue', $event)" />
  </div>
</template>

<script setup lang="ts">
import Toggle from '@/components/common/Toggle.vue'
defineProps<{ modelValue: boolean; label: string; hint?: string; disabled?: boolean; testId?: string }>()
defineEmits<{ 'update:modelValue': [value: boolean] }>()
</script>
