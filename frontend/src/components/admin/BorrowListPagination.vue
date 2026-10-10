<template>
  <div class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-200 px-4 py-3 text-xs dark:border-dark-600">
    <div class="flex flex-wrap items-center gap-2 text-gray-500 dark:text-gray-400">
      <span>{{ t('ticketBorrowing.pageInfo', { page, pages, count: total }) }}</span>
      <label class="flex items-center gap-2">{{ t('pagination.perPage') }}
        <Select :model-value="pageSize" :options="[20, 50, 100].map(value => ({ value, label: String(value) }))" :searchable="false" class="borrow-page-size" :aria-label="t('pagination.perPage')" @update:model-value="resize(Number($event))" />
      </label>
    </div>
    <div class="flex items-center gap-2">
      <button type="button" class="btn btn-secondary btn-sm" :disabled="page <= 1" @click="$emit('update:page', page - 1)">{{ t('pagination.previous') }}</button>
      <label class="flex items-center gap-1"><span class="sr-only">{{ t('pagination.jumpTo') }}</span><input v-model="jump" class="input !w-14 !rounded-lg !px-2 !py-1.5 text-center text-xs" type="number" min="1" :max="pages" :placeholder="String(page)" :aria-label="t('pagination.jumpTo')" @keydown.enter.prevent="go" /></label>
      <button type="button" class="btn btn-ghost btn-sm" @click="go">{{ t('pagination.jumpAction') }}</button>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="page >= pages" @click="$emit('update:page', page + 1)">{{ t('pagination.next') }}</button>
    </div>
  </div>
</template>
<script setup lang="ts">
import { computed, ref } from 'vue'
import Select from '@/components/common/Select.vue'
import { useI18n } from 'vue-i18n'
const props = defineProps<{ total: number; page: number; pageSize: number }>()
const emit = defineEmits<{ 'update:page': [value: number]; 'update:pageSize': [value: number] }>()
const { t } = useI18n()
const pages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)))
const jump = ref('')
function go() { const n = Number(jump.value); if (jump.value !== '' && Number.isInteger(n)) { emit('update:page', Math.max(1, Math.min(pages.value, n))); jump.value = '' } }
function resize(size: number) { emit('update:pageSize', size); emit('update:page', 1) }
</script>

<style scoped>
.borrow-page-size { @apply w-20; }
.borrow-page-size :deep(.select-trigger) { @apply rounded-lg px-2.5 py-1.5 text-xs; }
.borrow-page-size :deep(.select-value) { @apply min-w-0; }
</style>
