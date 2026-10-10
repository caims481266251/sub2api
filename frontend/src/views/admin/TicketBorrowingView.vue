<template>
  <AppLayout>
    <div class="borrow-page min-w-0 space-y-3">
      <SmartOpsNav />
      <header class="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-3">
        <div class="flex min-w-0 flex-wrap items-center gap-x-5 gap-y-2">
          <h1 class="text-lg font-semibold">{{ t('ticketBorrowing.title') }}</h1>
          <div class="flex flex-wrap gap-x-4 gap-y-2 text-xs text-gray-500">
            <button class="hover:text-primary-600" @click="tab = 'sources'">{{ t('ticketBorrowing.sources') }} <strong class="ml-1 text-gray-900 dark:text-gray-100">{{ sources.length }}</strong></button>
            <button class="hover:text-primary-600" @click="tab = 'targets'">{{ t('ticketBorrowing.management.availableTargets') }} <strong class="ml-1 text-emerald-600">{{ targets.filter(usable).length }} / {{ targets.length }}</strong></button>
            <button :class="backgroundRunning ? 'text-emerald-600' : 'text-amber-600'" @click="tab = 'settings'">{{ t(!savedSettings ? 'ticketBorrowing.statusUnknown' : backgroundRunning ? 'ticketBorrowing.running' : 'ticketBorrowing.paused') }}</button>
          </div>
        </div>
        <button class="btn btn-secondary btn-sm" :disabled="loading || busy" @click="reload">{{ t(loading ? 'ticketBorrowing.loading' : 'ticketBorrowing.refresh') }}</button>
      </header>
      <p v-if="error" role="alert" class="rounded-xl bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20">{{ error }}</p>
      <p v-if="pollError" role="status" class="text-xs text-amber-700 dark:text-amber-300">{{ t('ticketBorrowing.management.staleStatus') }}</p>
      <p v-if="notice" role="status" class="text-sm text-emerald-600">{{ notice }}</p>
      <nav class="borrow-tabs" :aria-label="t('ticketBorrowing.title')">
        <button v-for="item in tabs" :key="item" :aria-current="tab === item ? 'page' : undefined" class="borrow-tab" :class="{ 'borrow-tab-active': tab === item }" :data-tab="item" @click="tab = item">{{ t(`ticketBorrowing.${item}`) }}<span class="absolute right-1.5 top-1.5 h-1.5 w-1.5 rounded-full bg-amber-500" v-if="(item === 'sources' && (selectionDirty || rotationDirty)) || (item === 'settings' && settingsDirty) || (item === 'targets' && probeDirty)" :aria-label="t('ticketBorrowing.unsaved')"></span></button>
      </nav>

      <section v-show="tab === 'sources'" class="space-y-4">

        <div v-if="options.legacy_count" class="card space-y-3 border-amber-300 p-4">
          <p>{{ t('ticketBorrowing.restoreHint', { count: options.legacy_count }) }}</p>
          <p class="text-sm text-gray-500">{{ t('ticketBorrowing.restorePauseHint') }}</p>
          <div class="flex flex-wrap gap-2"><button class="btn btn-secondary" :disabled="busy || !savedSettings || backgroundRunning || !options.restorable_count" @click="restore">{{ t('ticketBorrowing.restore', { count: options.restorable_count }) }}</button><button class="btn btn-secondary" @click="tab = 'settings'">{{ t('ticketBorrowing.openSettings') }}</button></div>
        </div>
        <BorrowSourceList
          :sources="sources" :rotations="rotations" :groups="options.groups" :running="backgroundRunning"
          :busy="busy || loading" :rotation-loaded="rotationsLoaded" :selection-dirty="selectionDirty"
          v-model:selected="rotationSelection" @open="openSource" @rotate="rotateAccounts"
          @manage="sourceEditor = 'scope'" @defaults="sourceEditor = 'defaults'" @batch-settings="openBatchRotation"
        />
        <p v-if="selectionDirty" class="borrow-draft-banner" role="status">{{ t('ticketBorrowing.management.savedListHint') }} <button class="font-medium underline" @click="sourceEditor = 'scope'">{{ t('ticketBorrowing.management.resumeDraft') }}</button></p>
      </section>

      <section v-show="tab === 'targets'" class="space-y-3">


        <div v-if="!backgroundRunning" class="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-amber-50 p-4 text-sm text-amber-800 dark:bg-amber-900/20"><p>{{ t(savedSettings ? 'ticketBorrowing.pausedHint' : 'ticketBorrowing.settingsLoadRequired') }}</p><button class="btn btn-secondary" @click="tab = 'settings'">{{ t('ticketBorrowing.openSettings') }}</button></div>

        <BorrowTargetList :targets="targets" :running="backgroundRunning" :now="now + offset" :dirty-ids="probeDrafts" :busy="busy || loading || probeSaving" v-model:selected="probeSelection" @open="openTarget" @batch-settings="openBatchProbe"><RouterLink to="/admin/accounts" class="btn btn-secondary btn-sm">{{ t('ticketBorrowing.management.manageTargets') }}</RouterLink></BorrowTargetList>
      </section>

      <BorrowSettingsPanel v-if="tab === 'settings'" v-model="settings" :saved="savedSettings" :busy="busy || loading" :dirty="settingsDirty" :conflict="settingsConflict" :now="now + offset" @save="saveSettings" @revert="revertSettings" @targets="tab = 'targets'" />
      <BorrowEventList v-if="historyVisited" v-show="tab === 'events'" :visible="tab === 'events'" :events="visibleEvents" :sources="sources" :targets="targets" :has-older="hasOlder" :loading="eventsLoading || loading" @refresh="loadEvents(false)" @older="loadEvents(true)" />
      <details class="text-xs text-gray-500"><summary class="cursor-pointer">{{ t('ticketBorrowing.management.help') }}</summary><p class="mt-2 max-w-3xl leading-5">{{ t('ticketBorrowing.description') }} {{ t('ticketBorrowing.targetScope') }}</p></details>
    </div>
    <BorrowProbeBatchDialog v-if="batchProbeAccounts" :accounts="batchProbeAccounts" :selected-ids="batchProbeSelected" :busy="busy || loading" :running="backgroundRunning" :error="batchProbeError" :results="batchProbeResults" @save="saveBatchProbe" @close="closeBatchProbe" @dirty="batchProbeDirty = $event" />
    <BorrowRotationBatchDialog v-if="batchRotationAccounts" :accounts="batchRotationAccounts" :selected-ids="batchRotationSelected" :busy="busy || loading" :running="backgroundRunning" :error="batchRotationError" :results="batchRotationResults" @save="saveBatchRotation" @close="closeBatchRotation" @dirty="batchRotationDirty = $event" />
    <BaseDialog :show="!!sourceEditor" :title="t('ticketBorrowing.management.manageSources')" width="wide" placement="right" :show-close-button="!busy" :close-on-escape="!busy" @close="!busy && (sourceEditor = null)">
      <template v-if="sourceEditor">
        <div class="mb-4 flex flex-wrap gap-2"><button class="btn btn-sm" :class="sourceEditor === 'scope' ? 'btn-primary' : 'btn-secondary'" @click="sourceEditor = 'scope'">{{ t('ticketBorrowing.chooseScope') }}</button><button class="btn btn-sm" :class="sourceEditor === 'defaults' ? 'btn-primary' : 'btn-secondary'" @click="sourceEditor = 'defaults'">{{ t('ticketBorrowing.management.defaults') }}</button></div>
        <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
        <p class="mb-3 text-xs text-gray-500">{{ t('ticketBorrowing.management.drawerDraftHint') }}</p>
        <form id="borrow-selection-form" class="space-y-4" @submit.prevent="saveSelection">
          <fieldset v-if="sourceEditor === 'scope'" :disabled="busy || !selectionLoaded" class="card min-w-0 space-y-5 p-4 sm:p-5">
            <h2 class="font-semibold">{{ t('ticketBorrowing.chooseScope') }}</h2>
            <div class="grid gap-5 lg:grid-cols-2">
              <fieldset class="min-w-0 space-y-2">
                <legend class="font-medium">{{ t('ticketBorrowing.selectGroups') }}</legend>
                <p class="text-xs text-gray-500">{{ t('ticketBorrowing.groupsHint') }}</p>
                <input v-model="groupSearch" class="input" :placeholder="t('ticketBorrowing.searchGroups')" :aria-label="t('ticketBorrowing.searchGroups')" />
                <div class="max-h-56 space-y-1 overflow-y-auto rounded-lg border p-2 dark:border-dark-600 lg:h-56">
                  <label v-for="group in filteredGroups" :key="group.id" class="flex items-start gap-2 rounded p-2 text-sm hover:bg-gray-50 dark:hover:bg-dark-700">
                    <input v-model="selection.group_ids" :value="group.id" type="checkbox" class="mt-1" :data-group-id="group.id" /><span class="min-w-0 break-words">{{ group.name }} <span class="text-gray-400">#{{ group.id }}</span></span>
                  </label>
                  <p v-if="!filteredGroups.length" class="p-2 text-sm text-gray-500">{{ t(options.groups.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.noGroups') }}</p>
                </div>
              </fieldset>
              <fieldset class="min-w-0 space-y-2">
                <legend class="font-medium">{{ t('ticketBorrowing.selectAccounts') }}</legend>
                <p class="text-xs text-gray-500">{{ t('ticketBorrowing.accountSelectionHint') }}</p>
                <input v-model="search" class="input" :placeholder="t('ticketBorrowing.searchAccounts')" :aria-label="t('ticketBorrowing.searchAccounts')" />
                <div class="h-56 space-y-1 overflow-y-auto rounded-lg border p-2 dark:border-dark-600">
                  <label v-for="account in pagedCandidates" :key="account.id" class="flex items-start gap-2 rounded p-2 text-sm hover:bg-gray-50 dark:hover:bg-dark-700">
                    <input v-model="selection.account_ids" :value="account.id" type="checkbox" class="mt-1" :data-account-id="account.id" :disabled="account.is_target && !selection.account_ids.includes(account.id)" />
                    <span class="min-w-0 break-words">{{ account.name }} <span class="text-gray-400">#{{ account.id }}</span><span v-if="account.is_target" class="block text-xs text-amber-600">{{ t('ticketBorrowing.sourceIsTarget') }}</span><span v-else-if="inSelectedGroup(account)" class="block text-xs text-blue-600">{{ t('ticketBorrowing.includedByGroup') }}</span></span>
                  </label>
                  <p v-if="!filteredAccounts.length" class="p-2 text-sm text-gray-500">{{ t(options.accounts.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.emptySources') }}</p>
                </div>
                <BorrowListPagination v-if="filteredAccounts.length > candidatePageSize" v-model:page="candidatePage" v-model:page-size="candidatePageSize" :total="filteredAccounts.length" />
                <div class="flex flex-wrap gap-3 text-xs"><button type="button" class="text-primary-600" @click="selectMatchingAccounts">{{ t('ticketBorrowing.selectMatching', { count: filteredAccounts.filter(a => !a.is_target).length }) }}</button><button type="button" class="text-gray-500" @click="clearMatchingAccounts">{{ t('ticketBorrowing.clearMatching') }}</button></div>
              </fieldset>
            </div>
            <div v-if="missingGroups.length || missingAccounts.length" class="space-y-2 rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20">
              <p>{{ t('ticketBorrowing.missingSelection', { count: missingGroups.length + missingAccounts.length }) }}</p>
              <div class="flex flex-wrap gap-2"><button v-for="id in missingGroups" :key="`g${id}`" type="button" class="btn btn-secondary btn-sm" @click="selection.group_ids = selection.group_ids.filter(v => v !== id)">{{ t('ticketBorrowing.removeMissingGroup', { id }) }}</button><button v-for="id in missingAccounts" :key="`a${id}`" type="button" class="btn btn-secondary btn-sm" @click="selection.account_ids = selection.account_ids.filter(v => v !== id)">{{ t('ticketBorrowing.removeMissingAccount', { id }) }}</button></div>
            </div>
          </fieldset>
          <fieldset :disabled="busy || !selectionLoaded" class="card min-w-0 space-y-4 p-4 sm:p-5">
            <h2 class="font-semibold">{{ t('ticketBorrowing.configureModels') }}</h2>
            <div class="grid gap-5 md:grid-cols-2">
              <label class="block space-y-2"><span class="input-label">{{ t('ticketBorrowing.defaultModel') }}</span><input v-model="selection.default_model" class="input" required maxlength="256" data-testid="default-model" /><span class="block text-xs text-gray-500">{{ t('ticketBorrowing.defaultModelHint') }}</span></label>
              <label class="flex items-start gap-2 text-sm"><input v-model="selection.rotate_identity" class="mt-1" type="checkbox" :disabled="selection.use_proxy_pool" data-testid="rotate-identity" /><span>{{ t('ticketBorrowing.rotate') }}<span class="mt-2 block text-xs text-gray-500">{{ t('ticketBorrowing.proxyHint') }}</span></span></label>
            </div>
            <div class="space-y-2 rounded-lg border p-3 dark:border-dark-600">
              <label class="flex items-start gap-2 text-sm"><input v-model="selection.use_proxy_pool" type="checkbox" class="mt-1" data-testid="use-proxy-pool" @change="selection.use_proxy_pool && (selection.rotate_identity = false)" /><span>{{ t('ticketBorrowing.useProxyPool') }}</span></label>
              <p class="text-xs text-gray-500">{{ t('ticketBorrowing.poolBindingHint') }}</p>
              <RouterLink to="/admin/proxies#pool" class="text-sm text-primary-600">{{ t('ticketBorrowing.managePool') }}</RouterLink>
            </div>
            <div v-if="overrideCount" class="flex flex-wrap items-center justify-between gap-2 rounded-lg bg-blue-50 p-3 text-sm dark:bg-blue-900/20"><p>{{ t('ticketBorrowing.overrideCount', { count: overrideCount }) }}</p><button type="button" class="btn btn-secondary btn-sm" data-testid="reset-models" @click="resetModels">{{ t('ticketBorrowing.resetModels') }}</button></div>
          </fieldset>
          <fieldset :disabled="busy || !selectionLoaded" class="card min-w-0 overflow-hidden">
            <div class="space-y-3 p-4 sm:p-5">
              <h2 class="font-semibold">{{ t('ticketBorrowing.reviewSources') }}</h2>
              <p class="text-sm" data-testid="selection-count">{{ t('ticketBorrowing.selectionCount', { count: preview.length, groups: selection.group_ids.length, accounts: selection.account_ids.length }) }}</p>
              <p class="text-sm text-gray-500" data-testid="eligibility-count">{{ t('ticketBorrowing.eligibilityCount', { eligible: eligibleCount, excluded: preview.length - eligibleCount }) }}</p>
              <p class="text-xs text-gray-500">{{ t('ticketBorrowing.eligibilityHint') }}</p>
              <div class="flex flex-wrap gap-2"><input v-model="previewSearch" class="input sm:max-w-xs" :placeholder="t('ticketBorrowing.searchSelected')" :aria-label="t('ticketBorrowing.searchSelected')" /><Select v-model="previewFilter" class="w-full min-w-0 sm:max-w-xs" :aria-label="t('ticketBorrowing.filterSources')" :options="[{ value: 'all', label: t('ticketBorrowing.allSelected') }, { value: 'unavailable', label: t('ticketBorrowing.excludedSources') }, { value: 'overrides', label: t('ticketBorrowing.overriddenSources') }]" :searchable="false" :disabled="busy || !selectionLoaded" /></div>
            </div>
            <p v-if="visibleSources.length" class="px-4 pb-2 text-xs text-gray-500 sm:hidden">{{ t('ticketBorrowing.scrollTableHint') }}</p>
            <div v-if="visibleSources.length" class="overflow-x-auto"><table class="w-full min-w-[680px] text-left text-sm"><thead class="bg-gray-50 text-gray-500 dark:bg-dark-700"><tr><th v-for="column in ['name', 'selectionOrigin', 'eligibility', 'poolBinding', 'model']" :key="column" scope="col" class="p-3 font-medium">{{ t(`ticketBorrowing.${column}`) }}</th></tr></thead><tbody class="divide-y dark:divide-dark-600">
              <tr v-for="account in pagedSources" :key="account.id" :data-source-account-id="account.id">
                <td class="max-w-56 break-all p-3 align-top"><span class="font-medium">{{ account.name }}</span><span class="block text-xs text-gray-400">#{{ account.id }}</span></td>
                <td class="max-w-48 break-words p-3 align-top text-xs text-gray-500">{{ selectionOrigin(account) }}</td>
                <td class="max-w-56 p-3 align-top"><span :class="eligible(account) ? 'text-emerald-600' : 'text-amber-600'">{{ t(account.is_target ? 'ticketBorrowing.sourceIsTarget' : eligible(account) ? 'ticketBorrowing.eligibleSource' : 'ticketBorrowing.sourceUnavailable') }}</span><span v-if="sourceState(account.id)" class="mt-1 block text-xs text-gray-500">{{ t('ticketBorrowing.savedSourceResult') }}: {{ sourceState(account.id) }}</span></td>
                <td class="max-w-56 break-all p-3 align-top text-xs" :data-pool-binding="account.id"><template v-if="sourceBinding(account.id)?.use_proxy_pool"><span v-if="sourceBinding(account.id)?.pool_entry_id">#{{ sourceBinding(account.id)?.pool_entry_id }} · {{ sourceBinding(account.id)?.pool_endpoint }}<span class="mt-1 block text-gray-500">{{ t('ticketBorrowing.fixedFingerprint') }}: {{ sourceBinding(account.id)?.fingerprint_id }}</span></span><span v-else class="text-amber-600">{{ t('ticketBorrowing.poolBindingWaiting') }}</span></template><span v-else class="text-gray-500">{{ t('ticketBorrowing.accountProxy') }}</span></td>
                <td class="w-64 min-w-48 p-3 align-top"><div v-if="selection.model_overrides[account.id] !== undefined" class="space-y-1"><input :value="selection.model_overrides[account.id]" :placeholder="selection.default_model" :aria-label="t('ticketBorrowing.accountModel', { id: account.id })" class="input" maxlength="256" :data-model-account-id="account.id" @input="selection.model_overrides[account.id] = ($event.target as HTMLInputElement).value" /><button type="button" class="text-xs text-primary-600" @click="delete selection.model_overrides[account.id]">{{ t('ticketBorrowing.useDefault') }}</button></div><div v-else class="space-y-1"><span class="block break-all">{{ selection.default_model }}</span><span class="text-xs text-gray-500">{{ t('ticketBorrowing.inheritedModel') }} · </span><button type="button" class="text-xs text-primary-600" :data-customize-model="account.id" @click="selection.model_overrides[account.id] = selection.default_model">{{ t('ticketBorrowing.customizeModel') }}</button></div></td>
              </tr>
            </tbody></table></div>
            <p v-else class="p-6 text-center text-sm text-gray-500">{{ t(preview.length ? 'ticketBorrowing.noMatches' : 'ticketBorrowing.selectSourceHint') }}</p>
            <BorrowListPagination data-testid="source-preview-pagination" v-if="visibleSources.length" v-model:page="page" v-model:page-size="previewPageSize" :total="visibleSources.length" />
          </fieldset>
          <div class="space-y-2 rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-600 dark:bg-dark-800" :class="{ 'sm:sticky sm:bottom-3 sm:z-10 sm:shadow-lg': selectionDirty || selectionConflict }" data-testid="selection-save-bar">
            <p v-if="selectionConflict" role="alert" class="text-sm text-amber-600">{{ t('ticketBorrowing.remoteChange') }}</p>
            <div class="flex flex-wrap items-center justify-between gap-3"><p class="text-sm text-gray-500">{{ t(!selectionLoaded ? 'ticketBorrowing.configurationNotLoaded' : selectionDirty ? 'ticketBorrowing.unsavedHint' : 'ticketBorrowing.selectionSaved') }}</p><div class="borrow-save-actions"><button type="button" class="btn btn-ghost" :disabled="busy || !selectionLoaded || !preview.length" @click="clearSelection">{{ t('ticketBorrowing.clearSelection') }}</button><button type="button" class="btn btn-secondary" :disabled="busy || (!selectionDirty && !selectionConflict)" data-testid="revert-selection" @click="revertSelection">{{ t('ticketBorrowing.revert') }}</button><button type="submit" class="btn btn-primary" data-testid="save-selection" :disabled="busy || loading || !selectionLoaded || !selectionDirty || selectionConflict">{{ t('ticketBorrowing.saveSelection') }}</button></div></div>
          </div>
        </form>
      </template>
    </BaseDialog>
    <BaseDialog :show="!!activeSource" :title="activeSource ? `${activeSource.name || '#' + activeSource.account_id} · #${activeSource.account_id}` : ''" width="normal" placement="right" :show-close-button="!busy" :close-on-escape="!busy" @close="!busy && (sourceDetailId = null)">
      <template v-if="activeSource">
        <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
        <p v-if="rotationDrafts[activeSource.account_id]" class="borrow-draft-banner">{{ t('ticketBorrowing.management.drawerDraftHint') }}</p>
        <BorrowSourceRotation v-if="activeSource.use_proxy_pool" :key="activeSource.account_id" :account-id="activeSource.account_id" :name="activeSource.name || ''" :has-binding="!!activeSource.pool_entry_id" :state="rotationState(activeSource.account_id)" :editor-state="rotationEditorStates[activeSource.account_id]" :busy="busy" :running="backgroundRunning" :loaded="rotationsLoaded && !selectionDirty" @editor-state="rotationEditorStates[activeSource.account_id] = $event" @save="saveRotation(activeSource.account_id, $event)" @rotate="rotateAccounts([activeSource.account_id])" @dirty="rotationDrafts[activeSource.account_id] = $event" />
        <p v-else class="text-sm text-gray-500">{{ t('ticketBorrowing.accountProxy') }} · {{ t('ticketBorrowing.poolBindingHint') }}</p>
        <dl class="mt-5 grid gap-4 text-sm"><div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.model') }}</dt><dd class="mt-1 break-all">{{ activeSource.model }}</dd></div><div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.savedSourceResult') }}</dt><dd class="mt-1">{{ sourceState(activeSource.account_id) || '—' }}</dd></div></dl>
        <button class="btn btn-secondary mt-4" @click="previewSearch = String(activeSource.account_id); sourceDetailId = null; sourceEditor = 'defaults'">{{ t('ticketBorrowing.configureModels') }}</button>
      </template>
    </BaseDialog>
    <BaseDialog :show="!!activeTarget" :title="activeTarget ? `${activeTarget.name} · #${activeTarget.account_id}` : ''" width="wide" placement="right" :show-close-button="!probeSaving && !busy" :close-on-escape="!probeSaving && !busy" @close="!probeSaving && !busy && (targetDetailId = null)">
      <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
        <article v-for="target in activeTarget ? [activeTarget] : []" :key="target.account_id" class="min-w-0 space-y-4">
          <div class="flex flex-wrap items-start justify-between gap-3"><div class="min-w-0"><p class="break-all font-medium">{{ target.name }} <span class="text-gray-400">#{{ target.account_id }}</span></p><p class="mt-1 text-sm" :class="usable(target) ? 'text-emerald-600' : 'text-amber-600'">{{ t(usable(target) ? 'ticketBorrowing.ready' : 'ticketBorrowing.waiting') }}</p></div><span v-if="usable(target)" class="rounded-lg bg-emerald-50 px-3 py-1 text-sm text-emerald-700 dark:bg-emerald-900/20">{{ t('ticketBorrowing.remaining', { time: remaining(target) }) }}</span></div>
          <dl class="grid gap-4 rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700 sm:grid-cols-3"><div v-if="usable(target)" class="min-w-0"><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.gateway') }}</dt><dd class="mt-1 break-all">{{ target.gateway }}</dd></div><div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.phase') }}</dt><dd class="mt-1">{{ reason(target.phase) }}</dd></div><div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.reason') }}</dt><dd class="mt-1">{{ reason(target.reason) }}</dd></div></dl>
          <p v-if="target.scan_total" class="text-xs text-gray-500">{{ t('ticketBorrowing.scanProgress', { done: target.scan_completed, total: target.scan_total }) }} · {{ t('ticketBorrowing.scanProgressHint') }}</p>
          <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800" data-testid="recovery-status">
            <p v-if="!backgroundRunning && (target.inflight ?? 0) > 0" class="text-sm text-amber-600">{{ t('ticketBorrowing.draining') }}</p>
            <p v-else-if="usable(target)" class="text-sm text-emerald-600">{{ t(probeDue(target) ? 'ticketBorrowing.routeRenewing' : 'ticketBorrowing.validRouteIdle') }}</p>
            <p v-else-if="waitRemaining(target.upstream_wait_until) > 0" class="text-sm text-amber-600">{{ t('ticketBorrowing.upstreamWaitRemaining', { count: waitRemaining(target.upstream_wait_until) }) }} · {{ reason(target.upstream_wait_reason || 'target_upstream_waiting') }}</p>
            <p v-else-if="waitRemaining(target.budget_wait_until) > 0" class="text-sm text-amber-600">{{ t('ticketBorrowing.budgetWaitRemaining', { count: waitRemaining(target.budget_wait_until) }) }}</p>
            <dl class="mt-3 grid grid-cols-2 gap-x-4 gap-y-3 text-sm lg:grid-cols-5">
              <div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.idleTime') }}</dt><dd class="mt-1">{{ idleTime(target) }}</dd></div>
              <div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.inflight') }}</dt><dd class="mt-1">{{ target.inflight ?? 0 }}</dd></div>
              <div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.consecutiveFailures') }}</dt><dd class="mt-1">{{ target.consecutive_failures ?? 0 }}</dd></div>
              <div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.probeBudgetUsed') }}</dt><dd class="mt-1">{{ target.probe_requests_last_minute ?? 0 }} / {{ savedSettings?.recovery?.probe_requests_per_minute ?? '—' }}</dd></div>
              <div><dt class="text-xs text-gray-500">{{ t('ticketBorrowing.lastRequestFinished') }}</dt><dd class="mt-1">{{ target.last_request_finished_at ? new Date(target.last_request_finished_at).toLocaleTimeString() : '—' }}</dd></div>
            </dl>
            <p class="mt-3 text-xs text-gray-500">{{ t('ticketBorrowing.recoveryStatusHint') }}</p>
          </div>
          <p v-if="probeDrafts[target.account_id]" class="borrow-draft-banner">{{ t('ticketBorrowing.management.drawerDraftHint') }}</p>
          <BorrowProbePolicy :target="target" :editor-state="probeEditorStates[target.account_id]" @editor-state="probeEditorStates[target.account_id] = $event" @saving="probeSaving = $event" @saved="acceptProbeTarget" @dirty="probeDrafts[target.account_id] = $event" />
          <div class="flex flex-col gap-2 sm:flex-row sm:flex-wrap"><button class="btn btn-secondary" :title="t(!backgroundRunning ? 'ticketBorrowing.pausedHint' : usable(target) ? 'ticketBorrowing.validRouteIdle' : target.refresh_requested ? 'ticketBorrowing.queued' : 'ticketBorrowing.refreshRoute')" :disabled="busy || !backgroundRunning || usable(target) || target.refresh_requested" @click="refreshRoute(target.account_id)">{{ t(target.refresh_requested ? 'ticketBorrowing.queued' : 'ticketBorrowing.refreshRoute') }}</button><button class="btn btn-secondary" :disabled="busy || (!scanning(target) && (!backgroundRunning || usable(target)))" @click="scan(target)">{{ t(scanning(target) ? 'ticketBorrowing.cancelScan' : 'ticketBorrowing.scan') }}</button></div>
        </article>
    </BaseDialog>
  </AppLayout>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { onBeforeRouteLeave, RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import BorrowListPagination from '@/components/admin/BorrowListPagination.vue'
import BorrowSourceList from '@/components/admin/BorrowSourceList.vue'
import BorrowTargetList from '@/components/admin/BorrowTargetList.vue'
import type { BorrowEditorState } from '@/composables/useBorrowEditorState'
import type { BorrowProbePolicy as ProbePolicy } from '@/api/admin/ticketBorrowing'
import SmartOpsNav from '@/components/admin/operations/SmartOpsNav.vue'
import BorrowProbePolicy from '@/components/admin/BorrowProbePolicy.vue'
import BorrowSourceRotation from '@/components/admin/BorrowSourceRotation.vue'
import BorrowRotationBatchDialog from '@/components/admin/BorrowRotationBatchDialog.vue'
import BorrowProbeBatchDialog from '@/components/admin/BorrowProbeBatchDialog.vue'
import type { BorrowProbeBatchAccount, BorrowProbeBatchInput, BorrowProbeBatchResult } from '@/api/admin/ticketBorrowing'
import type { BorrowRotationBatchAccount, BorrowRotationBatchInput, BorrowRotationBatchResult } from '@/api/admin/ticketBorrowing'
import BorrowSettingsPanel from '@/components/admin/BorrowSettingsPanel.vue'
import BorrowEventList from '@/components/admin/BorrowEventList.vue'
import { ticketBorrowing as api, type BorrowSource, type BorrowTarget, type BorrowEvent, type BorrowSettings, type BorrowRecoverySettings, type BorrowSelection, type BorrowSourceOptions, type BorrowSourceOption, type BorrowRotationState, type BorrowRotationPolicy } from '@/api/admin/ticketBorrowing'
const { t, te } = useI18n()
const probeDrafts = ref<Record<number, boolean>>({})
const sourceEditor = ref<'scope' | 'defaults' | null>(null)
const sourceDetailId = ref<number | null>(null), targetDetailId = ref<number | null>(null)
const probeSaving = ref(false)
const rotationEditorStates = ref<Record<number, BorrowEditorState<BorrowRotationPolicy>>>({})
const probeEditorStates = ref<Record<number, BorrowEditorState<ProbePolicy>>>({})
const activeSource = computed(() => sources.value.find(s => s.account_id === sourceDetailId.value))
const activeTarget = computed(() => targets.value.find(s => s.account_id === targetDetailId.value))
function openSource(id: number) { sourceDetailId.value = id }
function openTarget(id: number) { targetDetailId.value = id }

function acceptProbeTarget(value: BorrowTarget) { targets.value = targets.value.map(target => target.account_id === value.account_id ? value : target) }
function probeDue(target: BorrowTarget) { return !!target.expires_at && Date.parse(target.expires_at) - now.value - offset <= (target.probe_policy?.renew_before_seconds ?? 15) * 1000 }
const tabs = ['sources', 'settings', 'targets', 'events'] as const
const tab = ref<(typeof tabs)[number]>('sources')
const historyVisited = ref(false)
watch(tab, value => { if (value === 'events') historyVisited.value = true })
watch(tab, () => { if (!probeSaving.value && !busy.value) { sourceEditor.value = null; sourceDetailId.value = null; targetDetailId.value = null } })
const rotations = ref<BorrowRotationState[]>([]), rotationsLoaded = ref(false), rotationSelection = ref<number[]>([]), rotationDrafts = ref<Record<number, boolean>>({})
const probeSelection = ref<number[]>([]), batchProbeAccounts = ref<BorrowProbeBatchAccount[] | null>(null)
const batchProbeSelected = ref<number[]>([]), batchProbeDirty = ref(false), batchProbeError = ref('')
const batchProbeResults = ref<BorrowProbeBatchResult[] | null>(null)
const batchRotationAccounts = ref<BorrowRotationBatchAccount[] | null>(null)
const batchRotationSelected = ref<number[]>([]), batchRotationDirty = ref(false), batchRotationError = ref('')
const batchRotationResults = ref<BorrowRotationBatchResult[] | null>(null)
const pendingRotationRequests = new Map<string, string>()
const sources = ref<BorrowSource[]>([]), targets = ref<BorrowTarget[]>([]), events = ref<BorrowEvent[]>([])
const defaultRecovery = (): BorrowRecoverySettings => ({ enabled: false, failure_threshold: 0, cooldown_seconds: [], probe_requests_per_minute: 12, source_requests_per_minute: 12 })
const settings = ref<BorrowSettings & { recovery: BorrowRecoverySettings }>({ running: false, ttl_seconds: 230, prewarm_seconds: 0, recovery: defaultRecovery() })
const selection = ref<BorrowSelection>({ revision: 0, group_ids: [], account_ids: [], default_model: 'gpt-6-luna', rotate_identity: true, use_proxy_pool: false, model_overrides: {} })
const savedSelection = ref<BorrowSelection>(), savedSettings = ref<BorrowSettings>()
const options = ref<BorrowSourceOptions>({ accounts: [], groups: [], legacy_count: 0, restorable_count: 0 })
const optionsLoaded = ref(false)
const clone = <T,>(value: T): T => JSON.parse(JSON.stringify(value))
function selectionKey(value: BorrowSelection) {
  return JSON.stringify({ groups: [...value.group_ids].sort((a,b) => a-b), accounts: [...value.account_ids].sort((a,b) => a-b), model: value.default_model, rotate: value.rotate_identity, pool: !!value.use_proxy_pool, overrides: Object.entries(value.model_overrides).sort(([a],[b]) => Number(a)-Number(b)) })
}
function settingsKey(value: BorrowSettings) { const r = value.recovery ?? defaultRecovery(); return JSON.stringify([value.running, value.ttl_seconds, r.probe_requests_per_minute, r.source_requests_per_minute ?? 12]) }
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
  const keepDraft = selectionDirty.value && !force
  savedSelection.value = clone(value)
  if (!keepDraft) { selection.value = clone(value); selectionBaseline.value = selectionKey(value) }
}
function acceptSettings(value: BorrowSettings, force = false) {
  const keepDraft = settingsDirty.value && !force
  savedSettings.value = clone(value)
  if (value.server_now) offset = Date.parse(value.server_now) - Date.now()
  if (!keepDraft) { settings.value = { ...clone(value), recovery: { ...defaultRecovery(), ...clone(value.recovery ?? {}) } }; settingsBaseline.value = settingsKey(value) }
}
function revertSelection() { if (savedSelection.value) acceptSelection(savedSelection.value, true) }
function revertSettings() { if (savedSettings.value) acceptSettings(savedSettings.value, true) }
const search = ref(''), groupSearch = ref(''), previewSearch = ref(''), previewFilter = ref('all'), page = ref(1)
const matches = (id: number, name: string, query: string) => `${id} ${name}`.toLowerCase().includes(query.trim().toLowerCase())
const filteredAccounts = computed(() => options.value.accounts.filter(a => matches(a.id, a.name, search.value)))
const candidatePage = ref(1), candidatePageSize = ref(20)
const pagedCandidates = computed(() => filteredAccounts.value.slice((candidatePage.value-1)*candidatePageSize.value, candidatePage.value*candidatePageSize.value))
watch([search, candidatePageSize], () => { candidatePage.value = 1 })
watch(() => filteredAccounts.value.length, count => { candidatePage.value = Math.min(candidatePage.value, Math.max(1, Math.ceil(count/candidatePageSize.value))) })
const filteredGroups = computed(() => options.value.groups.filter(g => matches(g.id, g.name, groupSearch.value)))
function inSelectedGroup(a: BorrowSourceOption) { return a.group_ids.some(id => selection.value.group_ids.includes(id)) }
const preview = computed(() => options.value.accounts.filter(a => selection.value.account_ids.includes(a.id) || inSelectedGroup(a)))
function eligible(a: BorrowSourceOption) { return !a.is_target && a.status === 'active' && a.schedulable }
const eligibleCount = computed(() => preview.value.filter(eligible).length)
const overrideCount = computed(() => preview.value.filter(a => selection.value.model_overrides[a.id] !== undefined).length)
const visibleSources = computed(() => preview.value.filter(a => matches(a.id, a.name, previewSearch.value) && (previewFilter.value === 'unavailable' ? !eligible(a) : previewFilter.value === 'overrides' ? selection.value.model_overrides[a.id] !== undefined : true)))
const previewPageSize = ref(20)
const pageCount = computed(() => Math.max(1, Math.ceil(visibleSources.value.length / previewPageSize.value)))
const pagedSources = computed(() => visibleSources.value.slice((page.value-1)*previewPageSize.value, page.value*previewPageSize.value))
watch([previewSearch, previewFilter, previewPageSize], () => { page.value = 1 })
watch(pageCount, value => { page.value = Math.min(page.value, value) })
const missingAccounts = computed(() => selection.value.account_ids.filter(id => !options.value.accounts.some(a => a.id === id)))
const missingGroups = computed(() => selection.value.group_ids.filter(id => !options.value.groups.some(g => g.id === id)))
function selectionOrigin(a: BorrowSourceOption) { const names = a.group_ids.filter(id => selection.value.group_ids.includes(id)).map(id => options.value.groups.find(g => g.id === id)?.name ?? `#${id}`); if (selection.value.account_ids.includes(a.id)) names.push(t('ticketBorrowing.explicitAccount')); return names.join(' · ') }
function clearSelection() { selection.value.group_ids = []; selection.value.account_ids = [] }
function selectMatchingAccounts() { selection.value.account_ids = [...new Set([...selection.value.account_ids, ...filteredAccounts.value.filter(a => !a.is_target).map(a => a.id)])] }
function clearMatchingAccounts() { const ids = new Set(filteredAccounts.value.map(a => a.id)); selection.value.account_ids = selection.value.account_ids.filter(id => !ids.has(id)) }
function resetModels() { for (const a of preview.value) delete selection.value.model_overrides[a.id] }
const sourceIndex = computed(() => new Map(sources.value.map(s => [s.account_id, s])))
function sourceBinding(id: number) { return sourceIndex.value.get(id) }
function sourceState(id: number) { const source = sourceBinding(id); return source && (source.unavailable_reason || source.last_reason) ? reason(source.unavailable_reason || source.last_reason) : '' }
const pollError = ref(false), eventsLoading = ref(false), oldestEventCursor = ref(0)
const loading = ref(false), busy = ref(false), error = ref(''), notice = ref(''), hasOlder = ref(false)
const now = ref(Date.now()); let offset = 0; let disposed = false; let polling = false; let refreshGeneration = 0
function reason(value: string) { const key = `ticketBorrowing.reasons.${value}`; return !value ? '—' : te(key) ? t(key) : t('ticketBorrowing.unknownResult') }
function failure(e: unknown) { const v = e as { reason?: string; message?: string }; const key = `ticketBorrowing.reasons.${v?.reason || v?.message}`; error.value = te(key) ? t(key) : t('ticketBorrowing.failure') }
function ttl(target: BorrowTarget) { return target.expires_at ? Date.parse(target.expires_at) - now.value - offset : 0 }
function usable(target: BorrowTarget) { return target.enabled && target.ready && ttl(target) > 0 }
function remaining(target: BorrowTarget) { return ttl(target) < 1000 ? t('ticketBorrowing.underSecond') : t('ticketBorrowing.seconds', { count: Math.ceil(ttl(target) / 1000) }) }
function waitRemaining(until?: string) { return until ? Math.max(0, Math.ceil((Date.parse(until) - now.value - offset) / 1000)) : 0 }
function idleTime(target: BorrowTarget) {
  if ((target.inflight ?? 0) > 0) return t('ticketBorrowing.activeRequests')
  return target.last_request_finished_at ? t('ticketBorrowing.seconds', { count: Math.max(0, Math.floor((now.value + offset - Date.parse(target.last_request_finished_at)) / 1000)) }) : '—'
}
function scanning(target: BorrowTarget) { return (target.scan_completed ?? 0) < (target.scan_total ?? 0) }
function acceptTargets(rows: BorrowTarget[]) { rows = rows.map(row => { const current = targets.value.find(target => target.account_id === row.account_id); return (current?.probe_policy?.revision ?? 0) > (row.probe_policy?.revision ?? 0) ? { ...row, probe_policy: current!.probe_policy } : row }); probeDrafts.value = Object.fromEntries(Object.entries(probeDrafts.value).filter(([id]) => rows.some(target => target.account_id === Number(id)))); targets.value = rows; if (rows[0]) offset = Date.parse(rows[0].server_now) - Date.now(); now.value = Date.now() }
function acceptRotations(rows: BorrowRotationState[]) {
  const current = new Map(rotations.value.map(row => [row.account_id, row]))
  rotations.value = rows.map(row => (current.get(row.account_id)?.revision ?? 0) > row.revision ? current.get(row.account_id)! : row)
  rotationsLoaded.value = true
}
async function reload() {
  if (loading.value) return
  loading.value = true
  refreshGeneration++
  try {
    const results = await Promise.allSettled([api.sources(), api.targets(), api.settings(), api.events(), api.selection(), api.sourceOptions(), api.rotations()])
    if (disposed) return
    const [ss, ts, config, es, selected, opts, rs] = results
    if (rs.status === 'fulfilled') acceptRotations(rs.value)
    if (ss.status === 'fulfilled') sources.value = ss.value
    if (ts.status === 'fulfilled') acceptTargets(ts.value)
    if (config.status === 'fulfilled') acceptSettings(config.value)
    if (es.status === 'fulfilled') acceptEvents(es.value)
    if (selected.status === 'fulfilled') acceptSelection(selected.value)
    if (opts.status === 'fulfilled') { options.value = opts.value; optionsLoaded.value = true }
    const failed = results.find(r => r.status === 'rejected')
    if (failed?.status === 'rejected') failure(failed.reason); else { error.value = ''; pollError.value = false }
  } finally { loading.value = false }
}
async function action(fn: () => Promise<unknown>, message = 'saved') {
  if (busy.value || loading.value) return
  busy.value = true; error.value = ''; notice.value = ''
  try { await fn(); notice.value = t(`ticketBorrowing.${message}`); await reload() }
  catch (e) { failure(e) }
  finally { busy.value = false }
}
async function saveSelection() {
  if (!selectionLoaded.value || !selectionDirty.value || selectionConflict.value || !selection.value.default_model.trim()) return
  await action(async () => {
    const value = { ...clone(selection.value), default_model: selection.value.default_model.trim(), group_ids: [...new Set(selection.value.group_ids)], account_ids: [...new Set(selection.value.account_ids)], model_overrides: Object.fromEntries(Object.entries(selection.value.model_overrides).map(([id,model]) => [id, model.trim()]).filter(([,model]) => model)) }
    acceptSelection(await api.saveSelection(value), true)
  })
}
async function restore() { if (window.confirm(t('ticketBorrowing.restoreConfirm'))) await action(() => api.restoreSources(), 'restored') }
async function saveSettings() {
  const { probe_requests_per_minute: target, source_requests_per_minute: source } = settings.value.recovery
  if (!Number.isInteger(settings.value.ttl_seconds) || settings.value.ttl_seconds < 30 || settings.value.ttl_seconds > 240 || !Number.isInteger(target) || target < 2 || target > 60 || !Number.isInteger(source) || source == null || source < 1 || source > 120) { error.value = t('ticketBorrowing.reasons.borrow_settings_invalid'); return }
  if (!savedSettings.value || !settingsDirty.value || settingsConflict.value) return
  await action(async () => { acceptSettings(await api.saveSettings(clone(settings.value)), true) })
}
async function refreshRoute(id: number) { if (backgroundRunning.value) await action(() => api.refreshTarget(id), 'routeQueued') }
async function scan(target: BorrowTarget) { const cancel = scanning(target); if ((!backgroundRunning.value && !cancel) || (!cancel && !window.confirm(t('ticketBorrowing.scanConfirm')))) return; await action(() => api.scanTarget(target.account_id, cancel), cancel ? 'scanCancelled' : 'scanQueued') }
const eventRetentionMS = 24 * 60 * 60 * 1000
const visibleEvents = computed(() => events.value.filter(event => Date.parse(event.created_at) >= now.value - eventRetentionMS))
function acceptEvents(rows: BorrowEvent[]) {
  // Restart the history cursor at the latest fetched batch. This fills gaps
  // after refreshing a busy stream, while keeping already loaded rows visible.
  if (rows.length) oldestEventCursor.value = Math.min(...rows.map(row => row.id))
  hasOlder.value = rows.length === 100
  const merged = new Map(events.value.map(event => [event.id, event]))
  for (const row of rows) merged.set(row.id, row)
  events.value = [...merged.values()].filter(event => Date.parse(event.created_at) >= Date.now() - eventRetentionMS).sort((a,b) => b.id-a.id)
}
async function loadEvents(older: boolean) {
  if (eventsLoading.value || loading.value) return
  eventsLoading.value = true
  const generation = refreshGeneration
  try {
    const rows = await api.events(older ? oldestEventCursor.value : 0)
    if (!disposed && generation === refreshGeneration) { acceptEvents(rows); error.value = '' }
  } catch (e) { if (!disposed && generation === refreshGeneration) failure(e) }
  finally { eventsLoading.value = false }
}
const rotationSources = computed(() => sources.value.filter(s => s.account_id > 0 && s.use_proxy_pool && s.unavailable_reason !== 'source_is_target'))
watch(rotationSources, rows => {
 rotationDrafts.value = Object.fromEntries(Object.entries(rotationDrafts.value).filter(([id]) => rows.some(s => s.account_id === Number(id))))
 rotationSelection.value = rotationSelection.value.filter(id => rows.some(s => s.account_id === id))
}, {immediate: true})
const rotationIndex = computed(() => new Map(rotations.value.map(r => [r.account_id, r])))
function rotationState(id: number) { return rotationIndex.value.get(id) }
const configurableTargets = computed(() => targets.value.filter(target => target.account_id > 0 && (target.probe_policy?.revision ?? 0) > 0))
watch(configurableTargets, rows => { probeSelection.value = probeSelection.value.filter(id => rows.some(target => target.account_id === id)) })
function openBatchProbe() {
 if (busy.value || loading.value || probeSaving.value || !configurableTargets.value.length) return
 if (probeDirty.value) { error.value = t('ticketBorrowing.probePolicy.bulk.draftHint'); return }
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
 busy.value = true; batchProbeError.value = ''; error.value = ''; notice.value = ''
 try {
  const results = await api.saveProbePolicies(input)
  batchProbeResults.value = results
  probeSelection.value = results.filter(item => item.status === 'failed').map(item => item.account_id)
  notice.value = t('ticketBorrowing.probePolicy.bulk.result', { saved: results.filter(item => item.status === 'saved').length, failed: probeSelection.value.length })
  await reload()
  if (error.value) batchProbeError.value = error.value
 } catch (e) {
  failure(e)
  batchProbeError.value = error.value
 } finally { busy.value = false }
}
function openBatchRotation() {
 if (busy.value || loading.value || selectionDirty.value || !rotationsLoaded.value || !rotationSources.value.length) return
 if (rotationDirty.value) { error.value = t('ticketBorrowing.rotation.bulk.draftHint'); return }
 // Freeze the account scope and revisions while the dialog is open. Polling must
 // not silently add accounts or rebase edits over another administrator's save.
 batchRotationAccounts.value = rotationSources.value.map(s => ({ account_id: s.account_id, name: s.name, revision: rotationState(s.account_id)?.revision ?? 0 }))
 batchRotationSelected.value = [...rotationSelection.value]
 batchRotationResults.value = null
 batchRotationError.value = ''
 error.value = ''
}
function closeBatchRotation() {
 if (busy.value || loading.value) return
 batchRotationAccounts.value = null
 batchRotationDirty.value = false
}
async function saveBatchRotation(input: BorrowRotationBatchInput) {
 if (busy.value || loading.value || selectionDirty.value || !batchRotationAccounts.value) return
 busy.value = true; batchRotationError.value = ''; error.value = ''; notice.value = ''
 try {
  const results = await api.saveRotationPolicies(input)
  batchRotationResults.value = results
  rotationSelection.value = results.filter(item => item.status === 'failed').map(item => item.account_id)
  notice.value = t('ticketBorrowing.rotation.bulk.result', { saved: results.filter(item => item.status === 'saved').length, failed: rotationSelection.value.length })
  await reload()
  if (error.value) batchRotationError.value = error.value
 } catch (e) {
  failure(e)
  batchRotationError.value = error.value
 } finally { busy.value = false }
}
async function rotateAccounts(ids: number[]) {
 const selected = [...ids].sort((a,b) => a-b)
 if (!selected.length || selectionDirty.value) return
 const key = `source:${selected.join(',')}`
 const request = pendingRotationRequests.get(key) ?? crypto.randomUUID()
 pendingRotationRequests.set(key, request)
 await action(async () => {
  await api.rotate('source', selected, request)
  pendingRotationRequests.delete(key)
  rotationSelection.value = []
 }, 'rotationQueued')
}
async function saveRotation(id: number, policy: BorrowRotationPolicy) {
 if (!Number.isInteger(policy.interval_seconds) || policy.interval_seconds < 1 || policy.interval_seconds > 86400) { error.value = t('ticketBorrowing.reasons.borrow_rotation_invalid'); return }
 await action(async () => { rotations.value = await api.saveRotationPolicy(id, policy) })
}
const rotationDirty = computed(() => Object.values(rotationDrafts.value).some(Boolean))
const probeDirty = computed(() => Object.values(probeDrafts.value).some(Boolean))
const hasUnsavedChanges = computed(() => selectionDirty.value || settingsDirty.value || rotationDirty.value || probeDirty.value || batchRotationDirty.value || batchProbeDirty.value)
function beforeUnload(event: BeforeUnloadEvent) {
 if (hasUnsavedChanges.value || ((batchRotationAccounts.value || batchProbeAccounts.value) && busy.value)) { event.preventDefault(); event.returnValue = '' }
}
onBeforeRouteLeave(() => {
 if ((batchRotationAccounts.value || batchProbeAccounts.value) && busy.value) return false
 return !hasUnsavedChanges.value || window.confirm(t('ticketBorrowing.leaveConfirm'))
})
const clock = setInterval(() => { now.value = Date.now() }, 1000)
async function refreshLiveState() {
  if (busy.value || loading.value || disposed || polling || document.visibilityState === 'hidden') return
  polling = true
  const generation = refreshGeneration
  try {
    const results = await Promise.allSettled([tab.value === 'targets' ? api.targets() : Promise.resolve(null), tab.value === 'sources' ? api.sources() : Promise.resolve(null), api.settings(), tab.value === 'sources' ? api.rotations() : Promise.resolve(null)])
    // A manual reload may have started while the poll was in flight.
    if (!disposed && !loading.value && !busy.value && generation === refreshGeneration) {
      if (results[0].status === 'fulfilled' && results[0].value) acceptTargets(results[0].value)
      if (results[1].status === 'fulfilled' && results[1].value) sources.value = results[1].value
      if (results[2].status === 'fulfilled') acceptSettings(results[2].value)
      if (results[3].status === 'fulfilled' && results[3].value) acceptRotations(results[3].value)
      pollError.value = results.some(result => result.status === 'rejected')
    }
  } finally { polling = false }
}
const poll = setInterval(refreshLiveState, 5000)
watch(tab, () => { void refreshLiveState() })
function visibilityChanged() { if (document.visibilityState === 'visible') void refreshLiveState() }
onMounted(() => { document.addEventListener('visibilitychange', visibilityChanged); window.addEventListener('beforeunload', beforeUnload); void reload() })
onBeforeUnmount(() => { document.removeEventListener('visibilitychange', visibilityChanged); disposed = true; clearInterval(clock); clearInterval(poll); window.removeEventListener('beforeunload', beforeUnload) })
</script>

<style scoped>
.borrow-draft-banner { @apply my-3 rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300; }

.borrow-tabs {
  @apply grid grid-cols-2 gap-1 rounded-xl border border-gray-200 bg-gray-100/80 p-1 dark:border-dark-700 dark:bg-dark-900;
  @apply min-[380px]:grid-cols-4 sm:inline-flex;
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
  @apply flex w-full flex-col gap-2 sm:w-auto sm:flex-row sm:flex-wrap;
}
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
