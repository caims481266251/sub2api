<template>
  <AppLayout>
    <div class="borrow-page min-w-0 space-y-3">
      <SmartOpsNav />
      <header class="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-3">
        <div class="flex min-w-0 flex-wrap items-center gap-x-5 gap-y-2">
          <h1 class="text-lg font-semibold">{{ t('ticketBorrowing.title') }}</h1>
          <div class="flex flex-wrap gap-x-4 gap-y-2 text-xs text-gray-500 dark:text-gray-400">
            <button class="hover:text-primary-600" @click="tab = 'sources'">{{ t('ticketBorrowing.management.eligibleSources') }} <strong class="ml-1 text-gray-900 dark:text-gray-100">{{ sources.filter(source => source.enabled && !source.unavailable_reason).length }} / {{ sources.length }}</strong></button>
            <button class="hover:text-primary-600" @click="tab = 'targets'">{{ t('ticketBorrowing.management.availableTargets') }} <strong class="ml-1 text-emerald-600">{{ targets.filter(usable).length }} / {{ targets.length }}</strong></button>
            <span>{{ t('ticketBorrowing.routes.summary', { count: targets.reduce((n, target) => n + (target.enabled ? target.business_routes?.filter(route => route.usable && route.inventory_eligible && borrowSeconds(route.usable_until, now + offset) > 0).length ?? 0 : 0), 0), inflight: targets.reduce((n, target) => n + (target.business_inflight ?? 0), 0) }) }}</span>
          </div>
        </div>
        <button class="btn btn-secondary btn-sm relative" :disabled="loading || busy" :title="pollError ? t('ticketBorrowing.management.staleStatus') : undefined" @click="reload">
          {{ t(loading ? 'ticketBorrowing.loading' : 'ticketBorrowing.refresh') }}
          <span v-if="pollError" class="absolute -right-1 -top-1 h-2.5 w-2.5 rounded-full bg-amber-500" aria-hidden="true"></span>
          <span v-if="pollError" class="sr-only" role="status">{{ t('ticketBorrowing.management.staleStatus') }}</span>
        </button>
      </header>
      <div class="flex flex-wrap items-center gap-x-6 gap-y-3 rounded-xl border bg-white p-3 dark:border-dark-700 dark:bg-dark-800">
        <button class="btn btn-secondary btn-sm" :disabled="busy || loading || controlsSaving || !savedSettings" data-testid="background-running" @click="saveControl('running', !backgroundRunning)">{{ t(backgroundRunning ? 'ticketBorrowing.simplified.pause' : 'ticketBorrowing.simplified.run') }}</button>
        <label class="flex items-center gap-3 text-sm">{{ t('ticketBorrowing.replenishment.title') }}<Toggle :model-value="savedSettings?.replenishment?.enabled === true" :disabled="busy || loading || controlsSaving || !savedSettings" :aria-label="t('ticketBorrowing.replenishment.enabled')" data-testid="replenishment-enabled" @update:model-value="saveControl('replenishment_enabled', $event)" /></label>
        <BorrowTag v-if="savedSettings?.replenishment?.enabled && !backgroundRunning" kind="state" value="replenishmentPaused" />
        <button class="btn btn-secondary btn-sm" data-testid="open-settings" @click="settingsOpen = true">{{ t('ticketBorrowing.management.parameters') }}<span v-if="settingsDirty" class="ml-1 text-amber-600">●</span></button>
        <span class="text-xs" :class="pollError ? 'text-amber-700' : 'text-gray-500'" role="status" data-testid="summary-freshness">{{ pollError ? t('ticketBorrowing.management.staleStatus') : lastUpdated ? t('ticketBorrowing.management.updatedAgo', { seconds: Math.max(0, Math.floor((now - lastUpdated) / 1000)) }) : t('ticketBorrowing.statusUnknown') }}</span>
        <p v-if="controlsError" role="alert" class="w-full text-sm text-red-600">{{ controlsError }}</p>
      </div>
      <nav class="borrow-tabs" :aria-label="t('ticketBorrowing.title')">
        <button v-for="item in tabs" :key="item" :aria-current="tab === item ? 'page' : undefined" class="borrow-tab" :class="{ 'borrow-tab-active': tab === item }" :data-tab="item" @click="tab = item">{{ t(`ticketBorrowing.${item}`) }}<span class="absolute right-1.5 top-1.5 h-1.5 w-1.5 rounded-full bg-amber-500" v-if="(item === 'sources' && (selectionDirty || Object.values(modelDrafts).some(Boolean))) || (item === 'targets' && probeDirty)" :aria-label="t('ticketBorrowing.unsaved')"></span></button>
      </nav>

      <section v-show="tab === 'sources'" class="space-y-4">

        <div v-if="options.legacy_count" class="card space-y-3 border-amber-300 p-4">
          <p>{{ t('ticketBorrowing.restoreHint', { count: options.legacy_count }) }}</p>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.restorePauseHint') }}</p>
          <div class="flex flex-wrap gap-2"><button class="btn btn-secondary" :disabled="busy || !savedSettings || backgroundRunning || !options.restorable_count" @click="restore">{{ t('ticketBorrowing.restore', { count: options.restorable_count }) }}</button><button class="btn btn-secondary" @click="settingsOpen = true">{{ t('ticketBorrowing.openSettings') }}</button></div>
        </div>
        <BorrowSourceList
          :sources="sources" :default-model="savedSelection?.default_model" :model-overrides="savedSelection?.model_overrides" :groups="options.groups" :running="backgroundRunning"
          :busy="busy || loading" :selection-dirty="selectionDirty"
          v-model:selected="sourceSelection" @open="openSource" @records="openRecords({ source_account_id: $event })"
          @remove="removeSources" :scope-loaded="selectionLoaded" @manage="sourceEditor = 'scope'" @defaults="sourceEditor = 'defaults'"
        />
        <p v-if="selectionDirty" class="borrow-draft-banner" role="status">{{ t('ticketBorrowing.management.savedListHint') }} <button class="font-medium underline" @click="sourceEditor = 'scope'">{{ t('ticketBorrowing.management.resumeDraft') }}</button></p>
      </section>

      <section v-show="tab === 'targets'" class="space-y-3">


        <div v-if="!backgroundRunning" class="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-amber-50 p-4 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300"><p>{{ t(savedSettings ? 'ticketBorrowing.pausedHint' : 'ticketBorrowing.settingsLoadRequired') }}</p></div>

        <BorrowTargetList :targets="targets" :running="backgroundRunning" :now="now + offset" :dirty-ids="probeDrafts" :source-budget="savedSettings?.source_budget" :busy="busy || loading || probeSaving" v-model:selected="probeSelection" @open="openTarget" @batch-settings="openBatchProbe" @records="(id, routeId) => openRecords({ target_id: id, route_id: routeId })"><button class="btn btn-secondary btn-sm" @click="addTargetOpen = true">{{ t('ticketBorrowing.management.addTarget') }}</button></BorrowTargetList>
      </section>

      <BaseDialog :show="settingsOpen" :title="t('ticketBorrowing.management.parameters')" width="wide" placement="right" @close="settingsOpen = false"><BorrowSettingsPanel v-if="settingsOpen" v-model="settings" :saved="savedSettings" :busy="busy || loading || controlsSaving" :dirty="settingsDirty" :conflict="settingsConflict" :now="now + offset" @save="saveSettings" @revert="revertSettings" @targets="settingsOpen = false; tab = 'targets'" @target="openTarget" :targets="targets" /></BaseDialog>
      <BorrowEventList v-if="historyVisited" v-show="tab === 'events'" :visible="tab === 'events'" v-model:query="eventQuery" :new-count="pendingEvents.length" @reading-history="readingHistory = $event" :events="visibleEvents" :sources="sources" :targets="targets" :has-older="hasOlder" :loading="eventsLoading || loading" @refresh="loadEvents(false)" @older="loadEvents(true)" />
      <details class="text-xs text-gray-500 dark:text-gray-400"><summary class="cursor-pointer">{{ t('ticketBorrowing.management.help') }}</summary><p class="mt-2 max-w-3xl leading-5">{{ t('ticketBorrowing.description') }} {{ t('ticketBorrowing.targetScope') }}</p></details>
    </div>
    <BaseDialog :show="addTargetOpen" :title="t('ticketBorrowing.management.addTarget')" @close="!busy && (addTargetOpen = false)">
      <Select v-model="newTargetId" :options="options.accounts.filter(a => !a.is_target).map(a => ({ value: a.id, label: `${a.name} #${a.id}` }))" :aria-label="t('ticketBorrowing.target')" :disabled="busy" />
      <p v-if="error" class="mt-3 text-red-600" role="alert">{{ error }}</p>
      <button class="btn btn-primary mt-4" :disabled="busy || !newTargetId" @click="addTarget">{{ t('ticketBorrowing.management.addTarget') }}</button>
    </BaseDialog>
    <BorrowProbeBatchDialog v-if="batchProbeAccounts" :accounts="batchProbeAccounts" :selected-ids="batchProbeSelected" :busy="busy || loading" :running="backgroundRunning" :error="batchProbeError" :results="batchProbeResults" @save="saveBatchProbe" @close="closeBatchProbe" @retry="retryBatchProbe" @dirty="batchProbeDirty = $event" />
    <BaseDialog :show="!!sourceEditor" :title="t('ticketBorrowing.management.manageSources')" width="wide" placement="right" :show-close-button="!busy" :close-on-escape="!busy" @close="!busy && (sourceEditor = null)">
      <template v-if="sourceEditor">
        <div class="mb-4 flex flex-wrap gap-2"><button class="btn btn-sm" :class="sourceEditor === 'scope' ? 'btn-primary' : 'btn-secondary'" @click="sourceEditor = 'scope'">{{ t('ticketBorrowing.chooseScope') }}</button><button class="btn btn-sm" :class="sourceEditor === 'defaults' ? 'btn-primary' : 'btn-secondary'" @click="sourceEditor = 'defaults'">{{ t('ticketBorrowing.management.defaults') }}</button></div>
        <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
        <p class="mb-3 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.management.drawerDraftHint') }}</p>
        <form id="borrow-selection-form" class="space-y-4" @submit.prevent="saveSelection">
          <BorrowSourceScopeEditor v-if="sourceEditor === 'scope'" :model-value="selection" :accounts="options.accounts" :groups="options.groups" :disabled="busy || loading || !selectionLoaded" @change="applyScope" />
          <fieldset v-if="sourceEditor === 'defaults'" :disabled="busy || loading || !selectionLoaded" class="card min-w-0 space-y-4 p-4 sm:p-5">
            <h2 class="font-semibold">{{ t('ticketBorrowing.configureModels') }}</h2>
            <div class="grid gap-5 md:grid-cols-2">
              <label class="block space-y-2"><span class="input-label">{{ t('ticketBorrowing.defaultModel') }}</span><input v-model="selection.default_model" class="input" required maxlength="256" data-testid="default-model" /><span class="block text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.defaultModelHint') }}</span></label>
              <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.boundExitRequired') }}</p>
            </div>
            <div class="space-y-2 rounded-lg border p-3 dark:border-dark-600">
              <p class="text-sm" data-testid="use-proxy-pool">{{ t('ticketBorrowing.useProxyPool') }}</p>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.poolBindingHint') }}</p>
              <RouterLink to="/admin/proxies#pool" class="text-sm text-primary-600">{{ t('ticketBorrowing.managePool') }}</RouterLink>
            </div>
            <div v-if="overrideCount" class="flex flex-wrap items-center justify-between gap-2 rounded-lg bg-blue-50 p-3 text-sm dark:bg-blue-900/20"><p>{{ t('ticketBorrowing.overrideCount', { count: overrideCount }) }}</p><button type="button" class="btn btn-secondary btn-sm" data-testid="reset-models" @click="resetModels">{{ t('ticketBorrowing.resetModels') }}</button></div>
          </fieldset>

          <details class="text-xs text-gray-500 dark:text-gray-400"><summary class="cursor-pointer">{{ t('ticketBorrowing.management.help') }}</summary><p class="my-3">{{ t('ticketBorrowing.scope.saveHint') }}</p><button type="button" class="btn btn-ghost" :disabled="busy || !selectionLoaded || !(selection.group_ids.length || selection.account_ids.length || selection.excluded_account_ids?.length)" data-testid="clear-source-scope" @click="clearSelection">{{ t('ticketBorrowing.clearSelection') }}</button></details>

        </form>
      </template>
      <template v-if="sourceEditor" #footer>
          <div class="space-y-2" data-testid="selection-save-bar">
            <p data-testid="scope-summary" class="text-sm">{{ t('ticketBorrowing.scope.summary', scopeSummary) }}</p>
            <p v-if="scopeError" role="alert" class="text-sm text-red-600">{{ scopeError }}</p>
            <button v-if="scopeUndo" type="button" class="text-sm text-primary-600" :disabled="busy || loading" data-testid="scope-undo" @click="undoScope">{{ t('ticketBorrowing.scope.undo') }}</button>
            <p v-if="selectionConflict" role="alert" class="text-sm text-amber-600">{{ t('ticketBorrowing.remoteChange') }}</p>
            <div class="flex flex-wrap items-center justify-between gap-3"><p class="text-sm text-gray-500 dark:text-gray-400">{{ t(!selectionLoaded ? 'ticketBorrowing.configurationNotLoaded' : selectionDirty ? 'ticketBorrowing.unsavedHint' : 'ticketBorrowing.selectionSaved') }}</p><div class="borrow-save-actions"><button type="button" class="btn btn-secondary" :disabled="busy || (!selectionDirty && !selectionConflict)" data-testid="revert-selection" @click="revertSelection">{{ t('ticketBorrowing.revert') }}</button><button type="submit" form="borrow-selection-form" class="btn btn-primary" data-testid="save-selection" :disabled="busy || loading || !selectionLoaded || !selectionDirty || selectionConflict">{{ t('ticketBorrowing.saveSelection') }}</button></div></div>
          </div>
      </template>
    </BaseDialog>
    <BaseDialog :show="!!activeSource" :title="activeSource ? `${activeSource.name || '#' + activeSource.account_id} · #${activeSource.account_id}` : ''" width="normal" placement="right" :show-close-button="!busy && !modelSaving" :close-on-escape="!busy && !modelSaving" @close="!busy && !modelSaving && (sourceDetailId = null)">
      <template v-if="activeSource">
        <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
        <p class="text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.simplified.randomEnvironment') }}</p>
        <dl class="mt-5 grid gap-4 text-sm"><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.tags.currentEligibility') }}</dt><dd class="mt-1"><BorrowTag :kind="activeSource.unavailable_reason ? 'reason' : 'state'" :value="activeSource.unavailable_reason || (activeSource.enabled ? 'eligible' : 'unavailable')" detail /></dd></div><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.model') }}</dt><dd class="mt-1 break-all">{{ activeSource.model }}</dd></div><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.savedSourceResult') }}</dt><dd class="mt-1"><BorrowTag :value="activeSource.last_reason" detail /></dd></div></dl>
        <BorrowReasonHint :code="activeSource.unavailable_reason || activeSource.last_reason || ''" />
        <button class="mt-2 text-sm text-primary-600" @click="openRecords({ source_account_id: activeSource.account_id })">{{ t('ticketBorrowing.management.relatedRecords') }}</button>
        <BorrowSourceModel v-if="savedSelection" :key="activeSource.account_id" :account-id="activeSource.account_id" :selection="savedSelection" :disabled="busy || selectionDirty" :editor-state="modelEditorStates[activeSource.account_id]" @editor-state="modelEditorStates[activeSource.account_id] = $event" @dirty="modelDrafts[activeSource.account_id] = $event" @saving="modelSaving = $event" @saved="modelSaved" />
      </template>
    </BaseDialog>
    <BaseDialog :show="!!activeTarget" :title="activeTarget ? `${activeTarget.name} · #${activeTarget.account_id}` : ''" width="wide" placement="right" :show-close-button="!probeSaving && !busy" :close-on-escape="!probeSaving && !busy" @close="!probeSaving && !busy && (targetDetailId = null)">
      <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
        <article v-for="target in activeTarget ? [activeTarget] : []" :key="target.account_id" class="min-w-0 space-y-4">
          <div class="flex flex-wrap items-start justify-between gap-3"><div class="min-w-0"><p class="break-all font-medium">{{ target.name }} <span class="text-gray-400">#{{ target.account_id }}</span></p><BorrowTag class="mt-1" kind="state" :value="!target.enabled ? 'disabled' : usable(target) ? 'ready' : 'noRoute'" /></div><span v-if="usable(target)" class="rounded-lg bg-emerald-50 px-3 py-1 text-sm text-emerald-700 dark:bg-emerald-900/20">{{ t('ticketBorrowing.management.admissionRemaining', { time: remaining(target) }) }}</span></div>
          <section class="space-y-3 rounded-lg border border-gray-100 p-4 dark:border-dark-700" data-testid="target-controls">
          <p v-if="probeDrafts[target.account_id]" class="borrow-draft-banner">{{ t('ticketBorrowing.management.drawerDraftHint') }}</p>
          <label class="flex items-center gap-3 text-sm">{{ t('ticketBorrowing.management.targetEnabled') }}<Toggle :model-value="target.enabled" :disabled="busy || probeSaving" :aria-label="t('ticketBorrowing.management.targetEnabled')" data-testid="target-enabled" @update:model-value="setTargetEnabled(target, $event)" /></label>
          <label v-if="target.enabled" class="block max-w-xs space-y-2"><span class="input-label">{{ t('ticketBorrowing.simplified.identity') }}</span><select class="input" :value="target.acquisition_identity ?? 'self'" :disabled="busy || probeSaving" data-testid="acquisition-identity" @change="setAcquisitionIdentity(target, ($event.target as HTMLSelectElement).value as 'self' | 'source')"><option value="self">{{ t('ticketBorrowing.simplified.self') }}</option><option value="source">{{ t('ticketBorrowing.simplified.source') }}</option></select></label>
          <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.simplified.environments') }}</p>
          <button v-if="target.enabled && target.acquisition_identity === 'source'" class="text-sm text-primary-600" @click="targetDetailId = null; tab = 'sources'">{{ t('ticketBorrowing.management.manageSources') }}</button>
          <button class="text-sm text-primary-600" @click="openRecords({ target_id: target.account_id })">{{ t('ticketBorrowing.management.relatedRecords') }}</button>
          <details><summary class="cursor-pointer text-sm">{{ t('ticketBorrowing.simplified.advanced') }}</summary><BorrowProbePolicy :target="target" :editor-state="probeEditorStates[target.account_id]" @editor-state="probeEditorStates[target.account_id] = $event" @saving="probeSaving = $event" @saved="acceptProbeTarget" @dirty="probeDrafts[target.account_id] = $event" /></details>
          </section>
          <dl class="grid gap-4 rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700 sm:grid-cols-3"><div v-if="usable(target)" class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.gateway') }}</dt><dd class="mt-1 break-all">{{ target.gateway }}</dd></div><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.phase') }}</dt><dd class="mt-1"><BorrowTargetStatus :target="target" :running="backgroundRunning" :now="now + offset" :source-budget="savedSettings?.source_budget" /></dd></div><div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.reason') }}</dt><dd class="mt-1"><BorrowTag :value="target.reason" detail /></dd></div></dl>
          <BorrowBusinessRoutes :target="target" :now="now + offset" @records="openRecords({ target_id: target.account_id, route_id: $event })" /><BorrowTargetStatistics :target-id="target.account_id" :observation="availability[target.account_id]" @records="openRecords({ target_id: target.account_id, route_id: $event })" />
          <p v-if="target.scan_total" class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.scanProgress', { done: target.scan_completed, total: target.scan_total }) }} · {{ t('ticketBorrowing.scanProgressHint') }}</p>
          <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800" data-testid="recovery-status">
            <dl class="mt-3 grid grid-cols-2 gap-x-4 gap-y-3 text-sm lg:grid-cols-5">
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.idleTime') }}</dt><dd class="mt-1">{{ idleTime(target) }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.inflight') }}</dt><dd class="mt-1">{{ target.inflight ?? 0 }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.consecutiveFailures') }}</dt><dd class="mt-1">{{ target.consecutive_failures ?? 0 }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.probeBudgetUsed') }}</dt><dd class="mt-1">{{ target.probe_requests_last_minute ?? 0 }} / {{ savedSettings?.recovery?.probe_requests_per_minute ?? '—' }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.lastRequestFinished') }}</dt><dd class="mt-1">{{ target.last_request_finished_at ? new Date(target.last_request_finished_at).toLocaleTimeString() : '—' }}</dd></div>
            </dl>
            <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ t('ticketBorrowing.recoveryStatusHint') }}</p>
          </div>
          <div class="flex flex-col gap-2 sm:flex-row sm:flex-wrap"><button class="btn btn-secondary" :title="t(!backgroundRunning ? 'ticketBorrowing.pausedHint' : usable(target) ? 'ticketBorrowing.validRouteIdle' : target.refresh_requested ? 'ticketBorrowing.queued' : 'ticketBorrowing.refreshRoute')" :disabled="busy || !backgroundRunning || (usable(target) && !savedSettings?.replenishment?.enabled) || !target.enabled || target.refresh_requested" @click="refreshRoute(target.account_id)">{{ t(target.refresh_requested ? 'ticketBorrowing.queued' : 'ticketBorrowing.refreshRoute') }}</button><button v-if="target.acquisition_identity === 'source'" class="btn btn-secondary" :disabled="busy || (!scanning(target) && (!backgroundRunning || usable(target)))" @click="scan(target)">{{ t(scanning(target) ? 'ticketBorrowing.cancelScan' : 'ticketBorrowing.scan') }}</button></div>
        </article>
    </BaseDialog>
  </AppLayout>
</template>
<script setup lang="ts">
import BorrowTag from '@/components/admin/BorrowTag.vue'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useBorrowPreference } from '@/composables/useBorrowPreference'
import { borrowReady, borrowSeconds } from '@/composables/borrowStatus'
import { observeBorrowAvailability, type BorrowAvailability } from '@/composables/borrowAvailability'
import BorrowTargetStatistics from '@/components/admin/BorrowTargetStatistics.vue'
import BorrowTargetStatus from '@/components/admin/BorrowTargetStatus.vue'
import BorrowBusinessRoutes from '@/components/admin/BorrowBusinessRoutes.vue'
import { onBeforeRouteLeave, useRoute, useRouter, RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import BorrowSourceModel, { type SourceModelDraft } from '@/components/admin/BorrowSourceModel.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import BorrowSourceList from '@/components/admin/BorrowSourceList.vue'
import BorrowTargetList from '@/components/admin/BorrowTargetList.vue'
import BorrowSourceScopeEditor from '@/components/admin/BorrowSourceScopeEditor.vue'
import { changeSourceScope, includedInScope, scopeWithinLimits } from '@/composables/borrowSourceScope'
import type { BorrowEditorState } from '@/composables/useBorrowEditorState'
import type { BorrowProbePolicy as ProbePolicy } from '@/api/admin/ticketBorrowing'
import SmartOpsNav from '@/components/admin/operations/SmartOpsNav.vue'
import BorrowProbePolicy from '@/components/admin/BorrowProbePolicy.vue'
import BorrowProbeBatchDialog from '@/components/admin/BorrowProbeBatchDialog.vue'
import type { BorrowProbeBatchAccount, BorrowProbeBatchInput, BorrowProbeBatchResult } from '@/api/admin/ticketBorrowing'
import BorrowSettingsPanel from '@/components/admin/BorrowSettingsPanel.vue'
import BorrowEventList from '@/components/admin/BorrowEventList.vue'
import BorrowReasonHint from '@/components/admin/BorrowReasonHint.vue'
import { ticketBorrowing as api, defaultBorrowReplenishment, type BorrowSource, type BorrowTarget, type BorrowEvent, type BorrowEventQuery, type BorrowSettings, type BorrowRecoverySettings, type BorrowSelection, type BorrowSourceOptions } from '@/api/admin/ticketBorrowing'
const { t, te } = useI18n()
const appStore = useAppStore()
const route = useRoute(), router = useRouter()
const probeDrafts = ref<Record<number, boolean>>({})
const sourceEditor = ref<'scope' | 'defaults' | null>(null)
const sourceDetailId = ref<number | null>(null), targetDetailId = ref<number | null>(null)
const probeSaving = ref(false), modelSaving = ref(false)
const modelDrafts = ref<Record<number, boolean>>({})
const modelEditorStates = ref<Record<number, BorrowEditorState<SourceModelDraft>>>({})
async function modelSaved(value: BorrowSelection) {
 acceptSelection(value, true)
 try { const rows = await api.sources(); if (!disposed) sources.value = rows }
 catch (e) { if (!disposed) { pollError.value = true; failure(e) } }
}
const probeEditorStates = ref<Record<number, BorrowEditorState<ProbePolicy>>>({})
const activeSource = computed(() => sources.value.find(s => s.account_id === sourceDetailId.value))
const activeTarget = computed(() => targets.value.find(s => s.account_id === targetDetailId.value))
function openSource(id: number) { sourceDetailId.value = id }
function openTarget(id: number) { settingsOpen.value = false; sourceDetailId.value = null; targetDetailId.value = id }

const addTargetOpen = ref(false), newTargetId = ref<number | null>(null)
async function setTargetEnabled(target: BorrowTarget, enabled: boolean) {
 await action(async () => { acceptProbeTarget(await api.setTarget(target.account_id, enabled, target.revision)) })
}
async function setAcquisitionIdentity(target: BorrowTarget, identity: 'self' | 'source') {
 await action(async () => { acceptProbeTarget(await api.setTarget(target.account_id, target.enabled, target.revision, identity, target.acquisition_revision ?? 0)) })
}
async function addTarget() {
 if (!newTargetId.value) return
 const id = newTargetId.value
 await action(async () => { const current = await api.target(id); await api.setTarget(id, true, current.revision); addTargetOpen.value = false; newTargetId.value = null })
}
function acceptProbeTarget(value: BorrowTarget) { targets.value = targets.value.map(target => target.account_id === value.account_id ? value : target) }
const tabs = ['targets', 'sources', 'events'] as const
const settingsOpen = ref(false)
const tab = useBorrowPreference<(typeof tabs)[number]>('tab', 'targets')
if (tabs.includes(route.query.tab as typeof tabs[number])) tab.value = route.query.tab as typeof tabs[number]
if (!tabs.includes(tab.value)) tab.value = 'targets'
const historyVisited = ref(tab.value === 'events')
watch(tab, value => { if (value === 'events') { historyVisited.value = true; if (!events.value.length) void loadEvents(false); else void checkNewEvents() } })
watch(sourceEditor, value => { if (value) { settingsOpen.value = false; sourceDetailId.value = null; targetDetailId.value = null } })
watch(settingsOpen, value => { if (value) { sourceEditor.value = null; sourceDetailId.value = null; targetDetailId.value = null } })
watch(tab, () => { if (!probeSaving.value && !busy.value) { settingsOpen.value = false; sourceEditor.value = null; sourceDetailId.value = null; targetDetailId.value = null } })
const sourceSelection = ref<number[]>([])
const probeSelection = ref<number[]>([]), batchProbeAccounts = ref<BorrowProbeBatchAccount[] | null>(null)
const batchProbeSelected = ref<number[]>([]), batchProbeDirty = ref(false), batchProbeError = ref('')
const batchProbeResults = ref<BorrowProbeBatchResult[] | null>(null)
const sources = ref<BorrowSource[]>([]), targets = ref<BorrowTarget[]>([]), events = ref<BorrowEvent[]>([])
const defaultRecovery = (): BorrowRecoverySettings => ({ enabled: false, failure_threshold: 0, cooldown_seconds: [], probe_requests_per_minute: 12, source_requests_per_minute: 12 })
const settings = ref<BorrowSettings & { recovery: BorrowRecoverySettings }>({ running: false, admission_window_seconds: 80, ttl_seconds: 80, continue_timeout_seconds: 7, min_route_remaining_seconds: 0, probe_concurrency: 2, renew_before_seconds: 15, prewarm_seconds: 0, recovery: defaultRecovery() })
const selection = ref<BorrowSelection>({ revision: 0, group_ids: [], account_ids: [], excluded_account_ids: [], default_model: 'gpt-6-luna', rotate_identity: false, use_proxy_pool: true, model_overrides: {} })
const savedSelection = ref<BorrowSelection>(), savedSettings = ref<BorrowSettings>()
const options = ref<BorrowSourceOptions>({ accounts: [], groups: [], legacy_count: 0, restorable_count: 0 })
const optionsLoaded = ref(false)
const clone = <T,>(value: T): T => JSON.parse(JSON.stringify(value))
function selectionKey(value: BorrowSelection) {
  return JSON.stringify({ groups: [...value.group_ids].sort((a,b) => a-b), accounts: [...value.account_ids].sort((a,b) => a-b), excluded: [...(value.excluded_account_ids ?? [])].sort((a,b) => a-b), model: value.default_model, rotate: value.rotate_identity, pool: !!value.use_proxy_pool, overrides: Object.entries(value.model_overrides).sort(([a],[b]) => Number(a)-Number(b)) })
}
function settingsKey(value: BorrowSettings) { const r = value.recovery ?? defaultRecovery(); return JSON.stringify([value.running, value.admission_window_seconds ?? Math.max(1, value.ttl_seconds - (value.min_route_remaining_seconds ?? 0)), value.continue_timeout_seconds ?? 7, value.probe_concurrency ?? 2, r.probe_requests_per_minute, r.source_requests_per_minute ?? 12, value.replenishment?.enabled ?? false]) }
// Keep the edit baseline separate from the latest server snapshot. A refresh must not
// replace a draft or silently advance its revision after another administrator's edit.
const selectionBaseline = ref(''), settingsBaseline = ref('')
const selectionDirty = computed(() => !!savedSelection.value && selectionKey(selection.value) !== selectionBaseline.value)
const settingsDirty = computed(() => !!savedSettings.value && settingsKey(settings.value) !== settingsBaseline.value)
const selectionConflict = computed(() => !!savedSelection.value && selection.value.revision !== savedSelection.value.revision)
const settingsConflict = computed(() => !!savedSettings.value && settings.value.revision !== savedSettings.value.revision)
const selectionLoaded = computed(() => !!savedSelection.value && optionsLoaded.value)
const backgroundRunning = computed(() => savedSettings.value?.running === true)
function acceptSelection(value: BorrowSelection, force = false) {
  if (value.revision < (savedSelection.value?.revision ?? 0)) return
  const keepDraft = selectionDirty.value && !force
  savedSelection.value = clone(value)
  if (!keepDraft) { selection.value = { ...clone(value), excluded_account_ids: [...(value.excluded_account_ids ?? [])], use_proxy_pool: true, rotate_identity: false }; scopeUndo.value = null; scopeError.value = ''; selectionBaseline.value = selectionKey(selection.value) }
}
function acceptSettings(value: BorrowSettings, force = false) {
  if ((value.revision ?? 0) < (savedSettings.value?.revision ?? 0)) return
  const keepDraft = settingsDirty.value && !force
  value = { ...value, admission_window_seconds: value.admission_window_seconds ?? Math.max(1, value.ttl_seconds - (value.min_route_remaining_seconds ?? 0)) }
  savedSettings.value = clone(value)
  if (value.server_now && Number.isFinite(Date.parse(value.server_now))) offset = Date.parse(value.server_now) - Date.now()
  if (!keepDraft) { settings.value = { ...clone(value), admission_window_seconds: value.admission_window_seconds ?? Math.max(1, value.ttl_seconds - (value.min_route_remaining_seconds ?? 0)), continue_timeout_seconds: value.continue_timeout_seconds ?? 7, min_route_remaining_seconds: value.min_route_remaining_seconds ?? 120, probe_concurrency: value.probe_concurrency ?? 2, renew_before_seconds: value.renew_before_seconds ?? 180, recovery: { ...defaultRecovery(), ...clone(value.recovery ?? {}) } }; settingsBaseline.value = settingsKey(value) }
}
const controlsSaving = ref(false), controlsError = ref('')
async function saveControl(field: 'running' | 'replenishment_enabled', enabled: boolean) {
  if (!savedSettings.value || controlsSaving.value || busy.value || loading.value) return
  const hadConflict = settingsConflict.value
  const hadDraft = settingsDirty.value
  controlsSaving.value = true; controlsError.value = ''; refreshGeneration++
  try {
    const value = await api.saveControls({ revision: savedSettings.value.revision ?? 0, [field]: enabled })
    if (disposed) return
    if (hadDraft && !hadConflict) {
      settings.value = { ...settings.value, revision: value.revision, running: value.running, replenishment: { ...(settings.value.replenishment ?? defaultBorrowReplenishment()), enabled: value.replenishment?.enabled ?? false } }
      savedSettings.value = clone(value)
      settingsBaseline.value = settingsKey(value)
    } else acceptSettings(value)
    appStore.showSuccess(t('ticketBorrowing.saved'))
    await refreshLiveState()
  } catch (e) {
    const detail = e as { reason?: string; message?: string }
    const key = `ticketBorrowing.reasons.${detail.reason || detail.message}`
    controlsError.value = te(key) ? t(key) : t('ticketBorrowing.failure')
    // Refresh only the server baseline; acceptSettings preserves any local draft.
    try { acceptSettings(await api.settings()) } catch { pollError.value = true }
  } finally { controlsSaving.value = false }
}
function revertSelection() { if (savedSelection.value) acceptSelection(savedSelection.value, true) }
function revertSettings() { if (savedSettings.value) acceptSettings(savedSettings.value, true) }
const preview = computed(() => options.value.accounts.filter(a => includedInScope(a, selection.value)))
const overrideCount = computed(() => preview.value.filter(a => selection.value.model_overrides[a.id] !== undefined).length)
const scopeUndo = ref<Pick<BorrowSelection, 'group_ids' | 'account_ids' | 'excluded_account_ids'> | null>(null)
const scopeError = ref('')
const scopeSnapshot = (value: BorrowSelection) => ({ group_ids: [...value.group_ids], account_ids: [...value.account_ids], excluded_account_ids: [...(value.excluded_account_ids ?? [])] })
function applyScope(value: BorrowSelection) {
  if (busy.value || loading.value || !selectionLoaded.value) return
  if (!scopeWithinLimits(value)) { scopeError.value = t('ticketBorrowing.scope.limit'); return }
  if (options.value.accounts.some(a => includedInScope(a, selection.value) && !includedInScope(a, value) && modelDrafts.value[a.id])) { scopeError.value = t('ticketBorrowing.scope.detailDraft'); return }
  scopeUndo.value = scopeSnapshot(selection.value)
  selection.value = value
  scopeError.value = ''
}
function undoScope() {
  if (!scopeUndo.value) return
  selection.value = { ...selection.value, ...scopeUndo.value }
  scopeUndo.value = null
  scopeError.value = ''
}
const scopeSummary = computed(() => {
  const before = new Set(options.value.accounts.filter(a => savedSelection.value && includedInScope(a, savedSelection.value)).map(a => a.id))
  const after = new Set(preview.value.map(a => a.id))
  return { added: [...after].filter(id => !before.has(id)).length, removed: [...before].filter(id => !after.has(id)).length, total: after.size, excluded: selection.value.excluded_account_ids?.length ?? 0 }
})
async function removeSources(ids: number[]) {
  if (busy.value || loading.value || !selectionLoaded.value) return
  if (ids.some(id => modelDrafts.value[id])) { reportError(t('ticketBorrowing.scope.detailDraft')); return }
  const frozenIds = [...ids]
  sourceEditor.value = 'scope'
  busy.value = true
  try {
    // Group membership can change while the saved-source list is polling.
    // Refresh options before deciding which removed accounts need exclusions.
    const latest = await api.sourceOptions()
    if (disposed) return
    options.value = latest
    busy.value = false
    applyScope(changeSourceScope(selection.value, latest.accounts, frozenIds, false))
    if (!scopeError.value) { sourceSelection.value = [] }
  } catch (e) { failure(e) }
  finally { busy.value = false }
}
function clearSelection() { applyScope({ ...selection.value, group_ids: [], account_ids: [], excluded_account_ids: [] }) }
function resetModels() { for (const a of preview.value) delete selection.value.model_overrides[a.id] }
const lastUpdated = ref<number | null>(null)
const pollError = ref(false), eventsLoading = ref(false), oldestEventCursor = ref(0)
const loading = ref(false), busy = ref(false), error = ref(''), hasOlder = ref(false)
const now = ref(Date.now()); let offset = 0; let disposed = false; let polling = false; let refreshGeneration = 0
function reportError(message: string) { error.value = message; appStore.showError(message) }
function failure(e: unknown) { const v = e as { reason?: string; message?: string }; const key = `ticketBorrowing.reasons.${v?.reason || v?.message}`; reportError(te(key) ? t(key) : t('ticketBorrowing.failure')) }
function ttl(target: BorrowTarget) { return borrowSeconds(target.usable_until ?? target.expires_at, now.value + offset) * 1000 }
function usable(target: BorrowTarget) { return borrowReady(target, now.value + offset) }
function remaining(target: BorrowTarget) { return ttl(target) < 1000 ? t('ticketBorrowing.underSecond') : t('ticketBorrowing.seconds', { count: Math.ceil(ttl(target) / 1000) }) }
function idleTime(target: BorrowTarget) {
  if ((target.inflight ?? 0) > 0) return t('ticketBorrowing.activeRequests')
  return target.last_request_finished_at ? t('ticketBorrowing.seconds', { count: Math.max(0, Math.floor((now.value + offset - Date.parse(target.last_request_finished_at)) / 1000)) }) : '—'
}
function scanning(target: BorrowTarget) { return (target.scan_completed ?? 0) < (target.scan_total ?? 0) }
const availability = ref<Record<number, BorrowAvailability>>({})
function acceptTargets(rows: BorrowTarget[]) { for (const row of rows) { const time = Date.parse(row.server_now); if (Number.isFinite(time)) availability.value[row.account_id] = observeBorrowAvailability(availability.value[row.account_id], row, time) } rows = rows.map(row => { const current = targets.value.find(target => target.account_id === row.account_id); if ((current?.revision ?? 0) > (row.revision ?? 0)) return current!;
 if ((current?.acquisition_revision ?? 0) > (row.acquisition_revision ?? 0)) row = { ...row, acquisition_identity: current!.acquisition_identity, acquisition_revision: current!.acquisition_revision };
 return (current?.probe_policy?.revision ?? 0) > (row.probe_policy?.revision ?? 0) ? { ...row, probe_policy: current!.probe_policy } : row }); probeDrafts.value = Object.fromEntries(Object.entries(probeDrafts.value).filter(([id]) => rows.some(target => target.account_id === Number(id)))); targets.value = rows; if (rows[0] && Number.isFinite(Date.parse(rows[0].server_now))) offset = Date.parse(rows[0].server_now) - Date.now(); now.value = Date.now() }
async function reload() {
  if (loading.value) return
  loading.value = true
  refreshGeneration++
  const eventsVersion = ++eventGeneration
  try {
    const results = await Promise.allSettled([api.sources(), api.targets(), api.settings(), api.events(eventQuery.value), api.selection(), api.sourceOptions()])
    if (disposed) return
    const [ss, ts, config, es, selected, opts] = results
    if (ss.status === 'fulfilled') sources.value = ss.value
    if (ts.status === 'fulfilled') acceptTargets(ts.value)
    if (config.status === 'fulfilled') acceptSettings(config.value)
    if (es.status === 'fulfilled' && eventsVersion === eventGeneration) acceptEvents(es.value)
    if (selected.status === 'fulfilled') acceptSelection(selected.value)
    if (opts.status === 'fulfilled') { options.value = opts.value; optionsLoaded.value = true }
    const failed = results.find(r => r.status === 'rejected')
    pollError.value = results.slice(0, 3).some(r => r.status === 'rejected')
    if (!pollError.value) lastUpdated.value = Date.now(); else for (const value of Object.values(availability.value)) value.lastAt = 0
    if (failed?.status === 'rejected') failure(failed.reason); else { error.value = '' }
  } finally { loading.value = false }
}
async function action(fn: () => Promise<unknown>, message = 'saved') {
  if (busy.value || loading.value) return
  busy.value = true; error.value = ''
  try { await fn(); if (message) appStore.showSuccess(t(`ticketBorrowing.${message}`)); await reload() }
  catch (e) { failure(e) }
  finally { busy.value = false }
}
async function saveSelection() {
  if (!scopeWithinLimits(selection.value)) { scopeError.value = t('ticketBorrowing.scope.limit'); return }
  if (!selectionLoaded.value || !selectionDirty.value || selectionConflict.value || !selection.value.default_model.trim()) return
  await action(async () => {
    const value = { ...clone(selection.value), default_model: selection.value.default_model.trim(), group_ids: [...new Set(selection.value.group_ids)], account_ids: [...new Set(selection.value.account_ids)], excluded_account_ids: [...new Set(selection.value.excluded_account_ids ?? [])], model_overrides: Object.fromEntries(Object.entries(selection.value.model_overrides).map(([id,model]) => [id, model.trim()]).filter(([,model]) => model)) }
    acceptSelection(await api.saveSelection(value), true)
  })
}
async function restore() { if (window.confirm(t('ticketBorrowing.restoreConfirm'))) await action(() => api.restoreSources(), 'restored') }
async function saveSettings() {
  const { admission_window_seconds: window, continue_timeout_seconds: timeout, probe_concurrency: concurrency } = settings.value
  const { probe_requests_per_minute: target, source_requests_per_minute: source } = settings.value.recovery
  const inRange = (value: unknown, min: number, max: number) => typeof value === 'number' && Number.isInteger(value) && value >= min && value <= max
  if (!inRange(window, 1, 240)) { reportError(t('ticketBorrowing.simplified.admissionError')); return }
  if (!inRange(timeout, 1, 45) || !inRange(concurrency, 1, 32) || !inRange(target, 2, 60) || !inRange(source, 1, 120)) { reportError(t('ticketBorrowing.reasons.borrow_settings_invalid')); return }
  if (!savedSettings.value || !settingsDirty.value || settingsConflict.value) return
  await action(async () => { acceptSettings(await api.saveSettings(clone(settings.value)), true) })
}
async function refreshRoute(id: number) { if (backgroundRunning.value) await action(() => api.refreshTarget(id), 'routeQueued') }
async function scan(target: BorrowTarget) { const cancel = scanning(target); if ((!backgroundRunning.value && !cancel) || (!cancel && !window.confirm(t('ticketBorrowing.scanConfirm')))) return; await action(() => api.scanTarget(target.account_id, cancel), cancel ? 'scanCancelled' : 'scanQueued') }
const eventRetentionMS = 24 * 60 * 60 * 1000
const eventQuery = ref<BorrowEventQuery>(queryFromRoute())
const pendingEvents = ref<BorrowEvent[]>([]), readingHistory = ref(false)
let eventGeneration = 0, eventDebounce: ReturnType<typeof setTimeout> | undefined
function queryFromRoute(): BorrowEventQuery {
 const q = route.query
 return { hide_rotations: q.rotations !== 'true', ...(Number(q.target) > 0 ? { target_id: Number(q.target) } : {}), ...(Number(q.source) > 0 ? { source_account_id: Number(q.source) } : {}), ...(Number(q.legacy_source) > 0 ? { source_id: Number(q.legacy_source) } : {}), ...(typeof q.route === 'string' ? { route_id: q.route } : {}), ...(typeof q.candidate === 'string' ? { candidate_id: q.candidate } : {}), ...(typeof q.stage === 'string' ? { stage: q.stage } : {}), ...(typeof q.reason === 'string' ? { reason: q.reason } : {}), ...(typeof q.search === 'string' ? { search: q.search } : {}), ...(typeof q.since === 'string' ? { since: q.since } : {}), ...(typeof q.until === 'string' ? { until: q.until } : {}) }
}
watch(() => route.query, () => {
 const value = queryFromRoute()
 if (JSON.stringify(Object.entries(value).filter(([,v]) => v !== undefined && v !== '').sort()) !== JSON.stringify(Object.entries(eventQuery.value).filter(([,v]) => v !== undefined && v !== '').sort())) eventQuery.value = value
 tab.value = tabs.includes(route.query.tab as typeof tabs[number]) ? route.query.tab as typeof tabs[number] : 'targets'
})
watch(eventQuery, () => {
 eventGeneration++; events.value = []; pendingEvents.value = []; oldestEventCursor.value = 0; hasOlder.value = false

 clearTimeout(eventDebounce)
 eventDebounce = setTimeout(() => { void loadEvents(false) }, 250)
}, { deep: true })
// One navigation write for tab + filters avoids competing replaces dropping a
// deep-link filter. History restores both; typing only replaces the current URL.
watch([tab, eventQuery], ([active, value]) => {
 const query = { tab: active, target: value.target_id ? String(value.target_id) : undefined, source: value.source_account_id ? String(value.source_account_id) : undefined, legacy_source: value.source_id ? String(value.source_id) : undefined, route: value.route_id || undefined, candidate: value.candidate_id || undefined, stage: value.stage || undefined, reason: value.reason || undefined, search: value.search || undefined, rotations: value.hide_rotations === false ? 'true' : undefined, since: value.since || undefined, until: value.until || undefined }
 const key = (q: Record<string, unknown>) => JSON.stringify(Object.entries(q).filter(([,v]) => v != null && v !== '').sort())
 if (key(query) === key(route.query)) return
 const navigate = (route.query.tab ?? 'targets') !== active ? router.push : router.replace
 void navigate({ query })
}, { deep: true, immediate: true })
function openRecords(query: BorrowEventQuery) {
 targetDetailId.value = null; sourceDetailId.value = null
 tab.value = 'events'
 eventQuery.value = { hide_rotations: true, ...query }
}
const visibleEvents = computed(() => events.value.filter(event => Date.parse(event.created_at) >= now.value - eventRetentionMS))
function acceptEvents(rows: BorrowEvent[]) {
 oldestEventCursor.value = rows.length ? Math.min(...rows.map(row => row.id)) : 0
 hasOlder.value = rows.length === 100
 const merged = new Map(events.value.map(event => [event.id, event]))
 for (const row of rows) merged.set(row.id, row)
 events.value = [...merged.values()].filter(event => Date.parse(event.created_at) >= Date.now() - eventRetentionMS).sort((a,b) => b.id-a.id)
}
async function loadEvents(older: boolean) {
 const generation = ++eventGeneration
 const query = { ...eventQuery.value, ...(older ? { before: oldestEventCursor.value } : {}) }
 eventsLoading.value = true
 try {
  const rows = await api.events(query)
  if (!disposed && generation === eventGeneration) { acceptEvents(rows); pendingEvents.value = []; error.value = '' }
 } catch (e) { if (!disposed && generation === eventGeneration) failure(e) }
 finally { if (generation === eventGeneration) eventsLoading.value = false }
}
async function checkNewEvents() {
 if (tab.value !== 'events' || eventsLoading.value || loading.value || busy.value || disposed || document.visibilityState === 'hidden') return
 if (!events.value.length) { await loadEvents(false); return }
 const generation = eventGeneration
 try {
  const after = events.value[0]!.id
  const rows = (await api.events({ ...eventQuery.value, after })).filter(row => row.id > after)
  if (disposed || generation !== eventGeneration || !rows.length) return
  if (readingHistory.value) pendingEvents.value = rows
  else await loadEvents(false)
 } catch { /* The explicit refresh keeps its own error feedback. */ }
}
watch(sources, rows => { sourceSelection.value = sourceSelection.value.filter(id => rows.some(source => source.account_id === id)) })
const configurableTargets = computed(() => targets.value.filter(target => target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0))
watch(configurableTargets, rows => { probeSelection.value = probeSelection.value.filter(id => rows.some(target => target.account_id === id)) })
function openBatchProbe() {
 if (busy.value || loading.value || probeSaving.value || !configurableTargets.value.length) return
 if (probeDirty.value) { reportError(t('ticketBorrowing.probePolicy.bulk.draftHint')); return }
 batchProbeAccounts.value = configurableTargets.value.map(target => ({ account_id: target.account_id, name: target.name, revision: target.probe_policy!.revision }))
 batchProbeSelected.value = [...probeSelection.value]
 batchProbeResults.value = null
 batchProbeError.value = ''
 error.value = ''
}
function closeBatchProbe() {
 if (busy.value || loading.value) return
 batchProbeAccounts.value = null
 batchProbeDirty.value = false
}
async function saveBatchProbe(input: BorrowProbeBatchInput) {
 if (busy.value || loading.value || probeSaving.value || !batchProbeAccounts.value) return
 busy.value = true; batchProbeError.value = ''; error.value = ''
 try {
  const results = await api.saveProbePolicies(input)
  batchProbeResults.value = results
  probeSelection.value = results.filter(item => item.status === 'failed').map(item => item.account_id)
  const summary = t('ticketBorrowing.probePolicy.bulk.result', { saved: results.filter(item => item.status === 'saved').length, failed: probeSelection.value.length })
  if (probeSelection.value.length) appStore.showWarning(summary); else appStore.showSuccess(summary)
  await reload()
  if (error.value) batchProbeError.value = error.value
 } catch (e) {
  failure(e)
  batchProbeError.value = error.value
 } finally { busy.value = false }
}
async function retryBatchProbe() {
 if (busy.value || loading.value || !batchProbeResults.value) return
 busy.value = true; batchProbeError.value = ''
 try {
  const failed = new Set(batchProbeResults.value.filter(r => r.status === 'failed').map(r => r.account_id))
  const rows = await api.targets()
  const retry = rows.filter(t => failed.has(t.account_id) && (t.probe_policy?.revision ?? 0) > 0)
  if (retry.length !== failed.size) throw new Error('borrow_configuration_conflict')
  acceptTargets(rows)
  batchProbeAccounts.value = retry.map(t => ({ account_id: t.account_id, name: t.name, revision: t.probe_policy!.revision }))
  batchProbeSelected.value = [...failed]; batchProbeResults.value = null
 } catch (e) { failure(e); batchProbeError.value = error.value }
 finally { busy.value = false }
}
const probeDirty = computed(() => Object.values(probeDrafts.value).some(Boolean))
const hasUnsavedChanges = computed(() => Object.values(modelDrafts.value).some(Boolean) || selectionDirty.value || settingsDirty.value || probeDirty.value || batchProbeDirty.value)
function beforeUnload(event: BeforeUnloadEvent) {
 if (hasUnsavedChanges.value || (batchProbeAccounts.value && busy.value)) { event.preventDefault(); event.returnValue = '' }
}
onBeforeRouteLeave(() => {
 if (batchProbeAccounts.value && busy.value) return false
 return !hasUnsavedChanges.value || window.confirm(t('ticketBorrowing.leaveConfirm'))
})
const clock = setInterval(() => { now.value = Date.now() }, 1000)
async function refreshLiveState() {
  if (busy.value || loading.value || disposed || polling || document.visibilityState === 'hidden') return
  polling = true
  const generation = refreshGeneration
  try {
    const results = await Promise.allSettled([api.targets(), api.sources(), api.settings()])
    // A manual reload may have started while the poll was in flight.
    if (!disposed && !loading.value && !busy.value && generation === refreshGeneration) {
      if (results[0].status === 'fulfilled' && results[0].value) acceptTargets(results[0].value)
      if (results[1].status === 'fulfilled' && results[1].value) sources.value = results[1].value
      if (results[2].status === 'fulfilled') acceptSettings(results[2].value)
      pollError.value = results.some(result => result.status === 'rejected')
      if (!pollError.value) lastUpdated.value = Date.now(); else for (const value of Object.values(availability.value)) value.lastAt = 0
    }
  } finally { polling = false }
}
const poll = setInterval(() => { void refreshLiveState(); void checkNewEvents() }, 5000)
watch(tab, () => { void refreshLiveState() })
function visibilityChanged() { if (document.visibilityState === 'visible') void refreshLiveState(); else for (const value of Object.values(availability.value)) value.lastAt = 0 }
onMounted(() => { document.addEventListener('visibilitychange', visibilityChanged); window.addEventListener('beforeunload', beforeUnload); void reload() })
onBeforeUnmount(() => { document.removeEventListener('visibilitychange', visibilityChanged); disposed = true; clearTimeout(eventDebounce); clearInterval(clock); clearInterval(poll); window.removeEventListener('beforeunload', beforeUnload) })
</script>

<style scoped>
.borrow-draft-banner { @apply my-3 rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300 dark:text-amber-300; }

.borrow-tabs {
  @apply grid grid-cols-2 gap-1 rounded-xl border border-gray-200 bg-gray-100/80 p-1 dark:border-dark-700 dark:bg-dark-900;
  @apply min-[380px]:grid-cols-3 sm:inline-flex;
}
.borrow-tab {
  @apply relative min-h-10 whitespace-nowrap rounded-lg px-3 py-2 text-sm font-medium leading-5 text-gray-600 transition-colors;
  @apply hover:bg-white/70 hover:text-gray-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500;
  @apply dark:text-gray-400 dark:hover:bg-dark-800 dark:hover:text-gray-100;
}
.borrow-tab-active {
  @apply bg-white text-primary-700 shadow-sm dark:bg-dark-700 dark:text-primary-300;
}
.borrow-save-actions {
  @apply flex w-full gap-2 sm:w-auto sm:flex-wrap;
}
.borrow-save-actions > button { @apply flex-1 sm:flex-none; }
.borrow-page :deep(input[type='checkbox']) {
  @apply h-4 w-4 shrink-0 cursor-pointer accent-primary-600 disabled:cursor-not-allowed;
}
.borrow-page :deep(.text-xs) {
  @apply leading-5;
}
.borrow-page :deep(.text-gray-500) {
  @apply dark:text-gray-400;
}
.borrow-page :deep(td) {
  @apply align-top;
}
</style>
