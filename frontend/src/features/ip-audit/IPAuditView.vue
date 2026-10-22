<template>
  <AppLayout>
    <div class="mx-auto max-w-[1600px] pb-8">
      <p class="mb-4 text-sm font-semibold">{{ t('nav.securityAudit') }}</p>
      <nav class="tabs inline-flex" role="tablist" :aria-label="t('nav.securityAudit')">
        <RouterLink to="/admin/risk-control" role="tab" class="tab" :aria-selected="false">{{ t('nav.contentModeration') }}</RouterLink>
        <RouterLink to="/admin/prompt-audit" role="tab" class="tab" :aria-selected="false">{{ t('nav.promptAudit') }}</RouterLink>
        <RouterLink to="/admin/ip-audit" role="tab" class="tab tab-active" :aria-selected="true">{{ t('ipAudit.title') }}</RouterLink>
      </nav>
      <div class="card mt-4 px-4 sm:px-6"><IPAuditPanel /></div>
    </div>
  </AppLayout>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import IPAuditPanel from './IPAuditPanel.vue'
const { t } = useI18n()
</script>
