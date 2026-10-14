<template>
  <div>
    <label class="input-label" :for="id">{{ label }}</label>
    <div
      class="flex items-center justify-between gap-3 rounded-lg border border-dashed px-4 py-3 transition-colors"
      :class="dragActive
        ? 'border-primary-400 bg-primary-50/70 dark:border-primary-500 dark:bg-primary-900/20'
        : 'border-gray-300 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'"
      data-testid="json-file-dropzone"
      @dragenter.prevent="handleDragEnter"
      @dragover.prevent
      @dragleave.prevent="handleDragLeave"
      @drop.prevent="handleDrop"
    >
      <div class="min-w-0">
        <div class="truncate text-sm text-gray-700 dark:text-dark-200" :title="fileListTitle">
          {{ selectedLabel || files[0]?.name || emptyLabel }}
        </div>
        <div class="text-xs text-gray-500 dark:text-dark-400">
          JSON (.json)
          <span v-if="files.length > 1"> · {{ fileListTitle }}</span>
        </div>
      </div>
      <button type="button" class="btn btn-secondary shrink-0" :disabled="disabled" @click="fileInput?.click()">
        {{ t('common.chooseFile') }}
      </button>
    </div>
    <input
      :id="id"
      ref="fileInput"
      type="file"
      class="hidden"
      accept="application/json,.json"
      :multiple="multiple"
      :disabled="disabled"
      @change="handleFileChange"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'

const props = withDefaults(defineProps<{
  id?: string
  label: string
  emptyLabel: string
  selectedLabel?: string
  files: File[]
  multiple?: boolean
  disabled?: boolean
}>(), { multiple: true, disabled: false })
const emit = defineEmits<{ select: [files: File[]] }>()
const { t } = useI18n()
const fileInput = ref<HTMLInputElement | null>(null)
const dragDepth = ref(0)
const dragActive = computed(() => !props.disabled && dragDepth.value > 0)
const fileListTitle = computed(() => props.files.map(file => file.name).join(', '))

function handleFileChange(event: Event) {
  const target = event.target as HTMLInputElement
  if (!props.disabled) emit('select', Array.from(target.files ?? []))
  target.value = ''
}
function handleDragEnter() { if (!props.disabled) dragDepth.value += 1 }
function handleDragLeave() { dragDepth.value = Math.max(0, dragDepth.value - 1) }
function handleDrop(event: DragEvent) {
  dragDepth.value = 0
  if (!props.disabled) emit('select', Array.from(event.dataTransfer?.files ?? []))
}
</script>
