<template>
  <section v-if="settings?.enabled || routeMode === 'hosted'" class="space-y-4">
    <fieldset>
      <legend class="input-label">{{ t('hosting.routeMode') }}</legend>
      <div class="grid grid-cols-2 gap-3">
        <label v-for="mode in routeModes" :key="mode.value" class="relative min-w-0 cursor-pointer">
          <input type="radio" name="hosting-route-mode" :value="mode.value" :checked="routeMode === mode.value" :disabled="mode.value === 'hosted' && !settings?.enabled" class="peer sr-only" @change="mode.value === 'hosted' ? selectHosted() : emit('update:routeMode', 'group')" />
          <span class="flex h-full items-start gap-3 rounded-xl border border-gray-200 bg-white p-3.5 transition-colors hover:border-primary-300 peer-checked:border-primary-500 peer-checked:bg-primary-50/60 peer-checked:ring-1 peer-checked:ring-primary-500 peer-focus-visible:outline peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-primary-500 peer-disabled:cursor-not-allowed peer-disabled:opacity-50 dark:border-dark-600 dark:bg-dark-800 dark:peer-checked:bg-primary-500/10">
            <Icon :name="mode.icon" size="md" class="mt-0.5 shrink-0 text-primary-600 dark:text-primary-400" />
            <span class="min-w-0"><span class="block text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t(mode.label) }}</span><span class="mt-1 block text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t(mode.hint) }}</span></span>
            <Icon v-if="routeMode === mode.value" name="check" size="sm" class="ml-auto mt-0.5 shrink-0 text-primary-600 dark:text-primary-400" />
          </span>
        </label>
      </div>
    </fieldset>
    <div v-if="routeMode === 'hosted'" class="overflow-hidden rounded-xl border border-gray-200 dark:border-dark-600">
      <div class="space-y-3 bg-gray-50/70 p-4 dark:bg-dark-800/60">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <label for="hosting-pool" class="input-label mb-0">{{ t('hosting.pool') }}</label>
          <span class="rounded-md bg-primary-50 px-2 py-1 text-xs font-medium tabular-nums text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">{{ t('hosting.keyServiceRate', { rate: settings?.rate_multiplier ?? '—' }) }}</span>
        </div>
        <select id="hosting-pool" class="input w-full" :value="poolId ?? 0" @change="emit('update:poolId', Number(($event.target as HTMLSelectElement).value))">
          <option v-for="pool in pools" :key="pool.id" :value="pool.id">{{ pool.id === 0 ? t('hosting.defaultPool') : pool.name }} · {{ t('hosting.poolAccounts', { count: pool.account_count }) }}</option>
        </select>
        <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('hosting.feeRule', { rate: settings?.rate_multiplier ?? '—' }) }}</p>
      </div>
      <div class="space-y-3 border-t border-gray-200 p-4 dark:border-dark-600">
        <div class="flex items-start justify-between gap-4">
          <div class="min-w-0"><p class="text-sm font-medium text-gray-900 dark:text-gray-100">{{ t('hosting.keyFallbackTitle') }}</p><p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('hosting.keyFallbackHint') }}</p></div>
          <Toggle class="mt-0.5" :model-value="fallbackEnabled" :aria-label="t('hosting.enableFallback')" data-testid="hosting-fallback-toggle" @update:model-value="emit('update:fallbackEnabled', $event)" />
        </div>
        <template v-if="fallbackEnabled">
          <div>
            <label for="hosting-fallback-group" class="sr-only">{{ t('hosting.fallbackGroup') }}</label>
            <select id="hosting-fallback-group" class="input w-full" :value="fallbackGroupId ?? ''" @change="emit('update:fallbackGroupId', Number(($event.target as HTMLSelectElement).value) || null)">
              <option value="">{{ t('hosting.selectFallback') }}</option>
              <option v-for="group in eligibleGroups" :key="group.id" :value="group.id">{{ group.name }} · {{ rates?.[group.id] ?? group.rate_multiplier }}×</option>
            </select>
          </div>
          <GroupBadge v-if="selectedGroup" :name="selectedGroup.name" :platform="selectedGroup.platform" :rate-multiplier="selectedGroup.rate_multiplier" :user-rate-multiplier="rates?.[selectedGroup.id]" :peak-rate-enabled="selectedGroup.peak_rate_enabled" :peak-start="selectedGroup.peak_start" :peak-end="selectedGroup.peak_end" :peak-rate-multiplier="selectedGroup.peak_rate_multiplier" />
          <div class="flex items-start gap-2 rounded-lg bg-gray-50 px-3 py-2.5 text-xs leading-5 text-gray-600 dark:bg-dark-800 dark:text-gray-400"><Icon name="infoCircle" size="sm" class="mt-0.5 shrink-0 text-gray-400" /><p>{{ t('hosting.fallbackPriceHint') }}</p></div>
        </template>
        <p v-else class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('hosting.noFallbackHint') }}</p>
      </div>
    </div>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Group } from '@/types'
import { hostingAPI, hostingFallbackGroups } from '@/api/hosting'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'

const props = defineProps<{ routeMode: 'group' | 'hosted'; poolId?: number | null; fallbackEnabled: boolean; fallbackGroupId: number | null; groups: Group[]; rates?: Record<number, number> }>()
const emit = defineEmits<{ 'update:routeMode': [value: 'group' | 'hosted']; 'update:poolId': [value: number]; 'update:fallbackEnabled': [value: boolean]; 'update:fallbackGroupId': [value: number | null] }>()
const routeModes = [
  { value: 'group', label: 'hosting.normalGroup', hint: 'hosting.keyGroupHint', icon: 'server' },
  { value: 'hosted', label: 'hosting.myPool', hint: 'hosting.keyHostedHint', icon: 'shieldKey' }
] as const
const { t } = useI18n()
const settings = ref<Awaited<ReturnType<typeof hostingAPI.status>>>()
const error = ref('')
const pools = computed(() => settings.value?.pools ?? [{ id: 0, name: '', account_count: 0, key_count: 0 }])
const eligibleGroups = computed(() => hostingFallbackGroups(props.groups))
const selectedGroup = computed(() => eligibleGroups.value.find(group => group.id === props.fallbackGroupId))
function selectHosted() {
  emit('update:routeMode', 'hosted')
  emit('update:fallbackEnabled', true)
  emit('update:fallbackGroupId', null)
}
onMounted(async () => {
  try { settings.value = await hostingAPI.status() } catch { if (props.routeMode === 'hosted') error.value = t('hosting.loadFailed') }
})
</script>
