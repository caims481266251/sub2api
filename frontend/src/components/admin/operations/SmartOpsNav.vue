<template>
  <nav ref="navigation" :aria-label="t('accountOps.smartTitle')" class="mb-5 flex w-fit max-w-full flex-nowrap gap-1 overflow-x-auto rounded-xl border border-gray-200/70 bg-white/80 p-1 shadow-sm dark:border-dark-700 dark:bg-dark-900">
    <RouterLink v-for="item in items" :key="item.path" :to="item.path" :aria-current="route.path === item.path ? 'page' : undefined" class="shrink-0 whitespace-nowrap rounded-lg px-4 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-primary-500" :class="route.path === item.path ? 'bg-gray-900 font-medium text-white dark:bg-gray-100 dark:text-gray-900' : 'text-gray-500 hover:bg-gray-100 dark:text-gray-400 dark:hover:bg-dark-800'">{{ t(item.label) }}</RouterLink>
  </nav>
</template>
<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAdminSettingsStore } from '@/stores/adminSettings'
const { t } = useI18n(), route = useRoute(), adminSettingsStore = useAdminSettingsStore()
const navigation = ref<HTMLElement>()
let resizeObserver: ResizeObserver | undefined
async function revealActiveItem() {
  await nextTick()
  const nav = navigation.value
  const active = nav?.querySelector<HTMLElement>('[aria-current="page"]')
  if (!nav || !active) return
  const item = active.getBoundingClientRect(), bounds = nav.getBoundingClientRect()
  if (item.left < bounds.left || item.right > bounds.right) nav.scrollLeft += item.left - bounds.left - (nav.clientWidth - item.width) / 2
}
onMounted(() => {
  void revealActiveItem()
  if (navigation.value && typeof ResizeObserver !== 'undefined') {
    resizeObserver = new ResizeObserver(() => { void revealActiveItem() })
    resizeObserver.observe(navigation.value)
  }
})
onBeforeUnmount(() => resizeObserver?.disconnect())
watch(() => route.path, revealActiveItem)
// Keep the order in sync with the 智能运维 group in AppSidebar.vue.
const items = computed(() => [{ path: '/admin/auto-config', label: 'autoConfig.title' }, { path: '/admin/priority-scheduling', label: 'priorityScheduling.title' }, { path: '/admin/account-quality', label: 'qualityOps.title' }, { path: '/admin/controlled-experiments', label: 'controlledExperiments.title' }, { path: '/admin/account-ops', label: 'accountOps.title' }, { path: '/admin/token-guard', label: 'tokenGuard.title' }, { path: '/admin/token-guard-v2', label: 'tokenGuardV2.title' }, { path: '/admin/pelican-tests', label: 'pelicanTests.title' }, ...(adminSettingsStore.requestCaptureEnabled ? [{ path: '/admin/request-captures', label: 'admin.requestCapture.title' }] : []), { path: '/admin/harvest-flow', label: 'nav.harvestFlow' }, { path: '/admin/ticket-borrowing', label: 'ticketBorrowing.title' }])
</script>
