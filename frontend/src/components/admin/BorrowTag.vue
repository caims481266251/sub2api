<template>
  <span class="borrow-tag-field" :data-tag-kind="kind" :data-tag-value="tag.code" :data-tag-tone="tag.tone">
    <span v-if="tag.code" class="borrow-tag" :class="`borrow-tag-${tag.tone}`" :title="title">{{ tag.literal || t(tag.key) }}</span>
    <span v-else class="text-gray-400">—</span>
    <span v-if="tag.code && !tag.known" class="borrow-tag-code">{{ tag.code }}</span>
    <span v-if="detail && description && description !== (tag.literal || t(tag.key))" class="borrow-tag-description">{{ description }}</span>
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { borrowTag, type BorrowTagKind } from '@/composables/borrowTags'

const props = withDefaults(defineProps<{ kind?: BorrowTagKind; value?: string | number; detail?: boolean }>(), { kind: 'reason', detail: false })
const { t, te } = useI18n()
const tag = computed(() => borrowTag(props.kind, props.value))
const description = computed(() => {
  const key = `ticketBorrowing.reasons.${tag.value.code}`
  return props.kind === 'reason' && tag.value.code && te(key) ? t(key) : ''
})
const title = computed(() => [description.value, props.kind === 'reason' || !tag.value.known ? tag.value.code : ''].filter(Boolean).join('\n'))
</script>

<style scoped>
.borrow-tag-field { @apply inline-flex min-w-0 max-w-full flex-col items-start gap-1 align-middle; }
.borrow-tag { @apply inline-flex max-w-full items-center rounded-md px-2 py-0.5 text-xs font-medium leading-5; overflow-wrap: anywhere; }
.borrow-tag-description { @apply text-xs font-normal leading-5 text-gray-500 dark:text-gray-400; overflow-wrap: anywhere; }
.borrow-tag-code { @apply text-xs font-normal text-gray-500 dark:text-gray-400; font-family: monospace; overflow-wrap: anywhere; }
.borrow-tag-success { @apply bg-emerald-50 text-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-300; }
.borrow-tag-info { @apply bg-blue-50 text-blue-700 dark:bg-blue-900/25 dark:text-blue-300; }
.borrow-tag-warning { @apply bg-amber-50 text-amber-800 dark:bg-amber-900/25 dark:text-amber-300; }
.borrow-tag-danger { @apply bg-red-50 text-red-700 dark:bg-red-900/25 dark:text-red-300; }
.borrow-tag-neutral { @apply bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300; }
.borrow-tag-category { @apply border border-violet-200 bg-violet-50/40 text-violet-700 dark:border-violet-800 dark:bg-violet-900/10 dark:text-violet-300; }
</style>
