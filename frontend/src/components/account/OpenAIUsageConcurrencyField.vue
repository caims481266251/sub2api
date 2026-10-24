<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { OpenAIUsageConcurrency } from '@/utils/openaiUsageConcurrency'
defineProps<{ baseConcurrency: number }>()
const model = defineModel<OpenAIUsageConcurrency>({ required: true })
const { t } = useI18n()
</script>

<template>
  <div class="rounded-xl border border-gray-200 p-4 dark:border-dark-600" data-testid="usage-concurrency">
    <label class="flex items-center gap-2 text-sm font-medium text-gray-800 dark:text-gray-100">
      <input v-model="model.enabled" type="checkbox" class="rounded" data-testid="usage-concurrency-enabled" />
      {{ t('admin.accounts.usageConcurrency.title') }}
    </label>
    <template v-if="model.enabled">
      <div class="mt-3 grid gap-4 sm:grid-cols-2">
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.usageConcurrency.threshold') }}</span>
          <div class="flex items-center gap-3">
            <input v-model.number="model.threshold_percent" type="range" min="0.1" max="100" step="0.1" class="min-w-0 flex-1" :aria-label="t('admin.accounts.usageConcurrency.threshold')" data-testid="usage-concurrency-slider" />
            <input v-model.number="model.threshold_percent" type="number" min="0.1" max="100" step="0.1" required class="input w-24" :aria-label="t('admin.accounts.usageConcurrency.threshold')" data-testid="usage-concurrency-threshold" />
            <span>%</span>
          </div>
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.usageConcurrency.limit') }}</span>
          <input v-model.number="model.concurrency" type="number" min="1" max="2147483647" step="1" required class="input" data-testid="usage-concurrency-limit" />
        </label>
      </div>
      <p class="input-hint mt-2">{{ t('admin.accounts.usageConcurrency.hint', { threshold: model.threshold_percent, low: Math.min(model.concurrency, baseConcurrency || model.concurrency), base: baseConcurrency }) }}</p>
      <p class="input-hint">{{ t('admin.accounts.usageConcurrency.usageHint') }}</p>
    </template>
  </div>
</template>
