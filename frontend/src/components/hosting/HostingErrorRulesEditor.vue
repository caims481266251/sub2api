<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import HostingSettingToggle from './HostingSettingToggle.vue'
import type { HostingErrorRuleDraft } from '@/utils/hostingErrorRules'
const enabled = defineModel<boolean>('enabled', { required: true })
const rules = defineModel<HostingErrorRuleDraft[]>('rules', { required: true })
const { t } = useI18n()
function move(index: number, offset: number) {
  const next = index + offset
  if (next < 0 || next >= rules.value.length) return
  const updated = [...rules.value]; [updated[index], updated[next]] = [updated[next], updated[index]]; rules.value = updated
}
function addRule() { rules.value.push({ error_code: 429, keywords: 'too many requests', duration_minutes: 30, description: '' }) }
</script>

<template>
  <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
    <HostingSettingToggle v-model="enabled" :label="t('admin.accounts.tempUnschedulable.title')" :hint="t('admin.accounts.tempUnschedulable.hint')" test-id="hosted-error-rules-enabled" />
    <template v-if="enabled">
      <p class="input-hint">{{ t('admin.accounts.tempUnschedulable.notice') }}</p>
      <fieldset v-for="(rule, index) in rules" :key="index" class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
        <legend class="px-1 text-sm">{{ t('admin.accounts.tempUnschedulable.ruleIndex', { index: index + 1 }) }}</legend>
        <div class="grid gap-3 sm:grid-cols-2"><label class="input-label">{{ t('admin.accounts.tempUnschedulable.errorCode') }}<input v-model.number="rule.error_code" class="input mt-1 w-full" type="number" min="400" max="599" step="1" required /></label><label class="input-label">{{ t('admin.accounts.tempUnschedulable.durationMinutes') }}<input v-model.number="rule.duration_minutes" class="input mt-1 w-full" type="number" min="1" max="10080" step="1" required /></label></div>
        <label class="input-label">{{ t('admin.accounts.tempUnschedulable.keywords') }}<input v-model="rule.keywords" class="input mt-1 w-full" :placeholder="t('admin.accounts.tempUnschedulable.keywordsPlaceholder')" required /></label>
        <p class="input-hint">{{ t('admin.accounts.tempUnschedulable.keywordsHint') }}</p>
        <label class="input-label">{{ t('admin.accounts.tempUnschedulable.description') }}<input v-model="rule.description" class="input mt-1 w-full" maxlength="256" /></label>
        <div class="flex flex-wrap gap-2"><button type="button" class="btn btn-secondary min-h-11" :disabled="index === 0" :aria-label="t('hosting.moveRuleUp')" @click="move(index, -1)">↑</button><button type="button" class="btn btn-secondary min-h-11" :disabled="index === rules.length - 1" :aria-label="t('hosting.moveRuleDown')" @click="move(index, 1)">↓</button><button type="button" class="btn btn-secondary min-h-11" @click="rules.splice(index, 1)">{{ t('common.delete') }}</button></div>
      </fieldset>
      <button type="button" class="btn btn-secondary min-h-11" :disabled="rules.length >= 20" data-testid="hosted-add-error-rule" @click="addRule">{{ t('admin.accounts.tempUnschedulable.addRule') }}</button>
    </template>
  </section>
</template>
