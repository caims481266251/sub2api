<template>
  <div v-if="draft" class="space-y-5">
    <fieldset :disabled="disabled" class="min-w-0 space-y-5">
      <div>
        <label for="hosted-name" class="input-label">{{ t('admin.accounts.accountName') }}</label>
        <input id="hosted-name" v-model="draft.name" class="input" maxlength="100" required :placeholder="t('admin.accounts.enterAccountName')" data-testid="hosted-name" />
      </div>
      <div>
        <label for="hosted-notes" class="input-label">{{ t('admin.accounts.notes') }}</label>
        <textarea id="hosted-notes" v-model="draft.notes" class="input" rows="3" maxlength="2000" :placeholder="t('admin.accounts.notesPlaceholder')" data-testid="hosted-notes" />
        <p class="input-hint">{{ t('admin.accounts.notesHint') }}</p>
      </div>
      <slot name="identity" />
      <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
        <HostingSettingToggle v-model="aliases" :label="t('priorityScheduling.modelAliases')" :hint="t('priorityScheduling.modelAliasesHint')" test-id="hosted-model-aliases" />
      </div>
      <section class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
        <h3 class="input-label">{{ t('admin.accounts.modelRestriction') }}</h3>
        <p v-if="flags.openai_passthrough" class="rounded-lg bg-amber-50 p-3 text-xs text-amber-700 dark:bg-amber-900/20 dark:text-amber-300">{{ t('hosting.mappingPassthroughHint') }}</p>
        <template v-else>
          <div class="mb-4 flex gap-2"><button v-for="tab in ['whitelist', 'mapping']" :key="tab" type="button" class="flex-1 rounded-lg px-4 py-2 text-sm font-medium transition-all" :class="modelTab === tab ? 'bg-primary-100 text-primary-700 dark:bg-primary-900/30 dark:text-primary-400' : 'bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-400'" :aria-pressed="modelTab === tab" @click="modelTab = tab">{{ t(tab === 'whitelist' ? 'admin.accounts.modelWhitelist' : 'admin.accounts.modelMapping') }}</button></div>
          <ModelWhitelistSelector v-show="modelTab === 'whitelist'" v-model="allowedModels" :model-mappings="mappings" :sync-models="syncModels" platform="openai" />
          <div v-show="modelTab === 'mapping'" class="space-y-3"><p class="rounded-lg bg-purple-50 p-3 text-xs text-purple-700 dark:bg-purple-900/20 dark:text-purple-400">{{ t('admin.accounts.mapRequestModels') }}</p>
          <div v-for="(mapping, index) in mappings" :key="index" class="flex items-center gap-2">
            <input v-model="mapping.from" class="input min-w-0 flex-1" :aria-label="t('admin.accounts.fromModel')" :placeholder="t('admin.accounts.fromModel')" required />
            <Icon name="arrowRight" size="sm" class="shrink-0 text-gray-400" />
            <input v-model="mapping.to" class="input min-w-0 flex-1" :aria-label="t('admin.accounts.toModel')" :placeholder="t('admin.accounts.toModel')" required />
            <button type="button" class="rounded-lg p-2 text-red-500 transition-colors hover:bg-red-50 dark:hover:bg-red-900/20" :aria-label="t('common.delete')" @click="mappings.splice(index, 1)"><Icon name="trash" size="sm" /></button>
          </div>
          <button type="button" class="w-full rounded-lg border-2 border-dashed border-gray-300 px-4 py-2 text-sm text-gray-600 transition-colors hover:border-gray-400 dark:border-dark-500 dark:text-gray-400" @click="mappings.push({ from: '', to: '' })"><Icon name="plus" size="sm" class="mr-1 inline" />{{ t('admin.accounts.addMapping') }}</button>
          <div class="flex flex-wrap gap-2"><button v-for="preset in presets" :key="preset.label" type="button" :class="['rounded-lg px-3 py-1 text-xs transition-colors', preset.color]" @click="addPreset(preset.from, preset.to)">+ {{ preset.label }}</button></div></div>
        </template>
      </section>
      <HostingErrorRulesEditor v-model:enabled="tempRulesEnabled" v-model:rules="tempRules" />
      <section class="border-t border-gray-200 pt-4 dark:border-dark-600">
        <div><label class="input-label">{{ t('admin.accounts.accountSchedulingThresholdOverrideValue') }}</label><input v-model="schedulingThreshold" class="input" type="number" min="1" max="100" step="1" :placeholder="t('hosting.inherit')" data-testid="hosted-scheduling-threshold" /><p class="input-hint">{{ t('admin.accounts.accountSchedulingThresholdOverrideHint') }}</p></div>
      </section>
      <UpstreamRequestIdHeaderField v-model="upstreamRequestIdHeader" platform="openai" type="oauth" />
      <div class="grid grid-cols-2 gap-4 lg:grid-cols-3">
        <div><label class="input-label" for="hosted-concurrency">{{ t('admin.accounts.concurrency') }}</label><input id="hosted-concurrency" v-model.number="draft.concurrency" class="input" type="number" min="1" max="1000" step="1" required /></div>
        <div><label class="input-label" for="hosted-load-factor">{{ t('admin.accounts.loadFactor') }}</label><input id="hosted-load-factor" v-model.number="draft.load_factor" class="input" type="number" min="1" max="1000" step="1" :placeholder="String(draft.concurrency)" data-testid="hosted-load-factor" /><p class="input-hint">{{ t('admin.accounts.loadFactorHint') }}</p></div>
        <div><label class="input-label" for="hosted-priority">{{ t('admin.accounts.priority') }}</label><input id="hosted-priority" v-model.number="draft.priority" class="input" type="number" min="0" max="1000" step="1" required /><p class="input-hint">{{ t('admin.accounts.priorityHint') }}</p></div>
      </div>
      <section class="border-t border-gray-200 pt-4 dark:border-dark-600"><AccountRpmSettings v-model:enabled="rpmEnabled" v-model:base-rpm="baseRpm" strict /></section>
      <div class="border-t border-gray-200 pt-4 dark:border-dark-600"><label class="input-label" for="hosted-expires-at">{{ t('admin.accounts.expiresAt') }}</label><input id="hosted-expires-at" v-model="expiresAtInput" type="datetime-local" class="input" data-testid="hosted-expires-at" /><p class="input-hint">{{ t('admin.accounts.expiresAtHint') }}</p></div>
      <div class="space-y-5">
        <div v-for="field in enumFields" :key="field.key" class="flex flex-col gap-3 border-t border-gray-200 pt-4 dark:border-dark-600 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
          <div class="min-w-0 flex-1"><label class="input-label mb-0">{{ t(field.label) }}</label><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(field.hint) }}</p></div>
          <div class="w-full shrink-0 sm:w-44"><Select v-model="enums[field.key]" :options="field.options.map(option => ({ value: option.value, label: t(option.label) }))" :disabled="disabled" :aria-label="t(field.label)" :data-testid="`hosted-${field.key}`" /></div>
        </div>
        <HostingSettingToggle class="border-t border-gray-200 pt-4 dark:border-dark-600" v-for="field in protocolFlags" :key="field.key" v-model="flags[field.key]" :label="t(field.label)" :hint="field.hint ? t(field.hint) : undefined" :disabled="field.key === 'codex_cli_only_allow_app_server' && !flags.codex_cli_only" :test-id="`hosted-${field.key}`" />
        <div class="flex flex-col gap-3 border-t border-gray-200 pt-4 dark:border-dark-600 sm:flex-row sm:items-center sm:justify-between sm:gap-4"><div class="min-w-0 flex-1"><label class="input-label mb-0">{{ t('admin.accounts.openai.codexImageTool') }}</label><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.codexImageToolDesc') }}</p></div><div class="w-full shrink-0 sm:w-44"><Select v-model="imageMode" :options="imageOptions.map(option => ({ value: option.value, label: t(option.label) }))" :disabled="disabled" :aria-label="t('admin.accounts.openai.codexImageTool')" /></div></div>
      </div>
      <section class="flex flex-col gap-3 border-t border-gray-200 pt-4 dark:border-dark-600 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
        <div class="min-w-0 flex-1"><label class="input-label mb-0">{{ t('admin.accounts.openai.planType') }}</label><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.planTypeDesc') }}</p></div>
        <div class="w-full shrink-0 sm:w-44"><Select v-model="planType" :options="planOptions" :aria-label="t('admin.accounts.openai.planType')" /></div>
      </section>
      <section class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
        <div><label class="input-label">{{ t('admin.accounts.openai.compactModelMapping') }}</label><p class="input-hint">{{ t('admin.accounts.openai.compactModelMappingDesc') }}</p></div>
        <div v-for="(mapping, index) in compactMappings" :key="index" class="flex items-center gap-2"><input v-model="mapping.from" class="input min-w-0 flex-1" :aria-label="t('admin.accounts.fromModel')" :placeholder="t('admin.accounts.fromModel')" required /><span class="text-gray-400">→</span><input v-model="mapping.to" class="input min-w-0 flex-1" :aria-label="t('admin.accounts.toModel')" :placeholder="t('admin.accounts.toModel')" required /><button type="button" class="rounded-lg p-2 text-red-500 hover:bg-red-50 dark:hover:bg-red-900/20" :aria-label="t('common.delete')" @click="compactMappings.splice(index, 1)"><Icon name="trash" size="sm" /></button></div>
        <button type="button" class="btn btn-secondary text-sm" @click="compactMappings.push({ from: '', to: '' })">+ {{ t('admin.accounts.addMapping') }}</button>
      </section>
      <section v-if="bpsAvailable" class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
        <HostingSettingToggle v-model="flags.openai_excel_bps" :label="t('admin.accounts.openai.excelBPS')" :hint="t('admin.accounts.openai.excelBPSDesc')" test-id="hosted-bps-enabled" />
        <template v-if="flags.openai_excel_bps">
          <HostingSettingToggle v-model="bpsAllModels" :label="t('admin.accounts.openai.excelBPSAllModels')" />
          <ModelWhitelistSelector v-if="!bpsAllModels" v-model="bpsModels" platform="openai" />
          <button v-if="!bpsAllModels" type="button" class="btn btn-secondary" @click="bpsModels = ['gpt-6-astra']">{{ t('admin.accounts.openai.excelBPSAstraOnly') }}</button>
          <HostingSettingToggle v-for="field in bpsFlags" :key="field.key" v-model="flags[field.key]" :label="t(field.label)" :hint="t(field.hint)" :disabled="field.key === 'openai_excel_bps_auto_recover_on_403' && !flags.openai_excel_bps_auto_disable_on_403" />
          <div v-if="flags.openai_excel_bps_auto_disable_on_403 && flags.openai_excel_bps_auto_recover_on_403"><label class="input-label">{{ t('admin.accounts.openai.excelBPS403RecoveryInterval') }}</label><input v-model.number="bpsRecoveryMinutes" class="input" type="number" min="1" max="10080" step="1" required /></div>
        </template>
      </section>
      <section class="border-t border-gray-200 pt-4 dark:border-dark-600">
        <HostingSettingToggle :model-value="draft.auto_pause_on_expired ?? true" @update:model-value="draft.auto_pause_on_expired = $event" :label="t('admin.accounts.autoPauseOnExpired')" :hint="t('admin.accounts.autoPauseOnExpiredDesc')" />
      </section>
      <section class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
        <div v-for="window in ['5h', '7d'] as const" :key="window" class="space-y-4">
          <HostingSettingToggle v-model="flags[`auto_pause_${window}_disabled`]" :label="t(`admin.accounts.autoPause${window}Disabled`)" :hint="t('admin.accounts.autoPauseDisabledHint')" />
          <div><label class="input-label" :for="`hosted-threshold-${window}`">{{ t(`admin.accounts.autoPause${window}Threshold`) }}</label><input :id="`hosted-threshold-${window}`" v-model="thresholds[window]" class="input" type="number" min="0.1" max="100" step="0.1" :placeholder="t('hosting.inherit')" /><p class="input-hint">{{ t('admin.accounts.autoPauseThresholdHint') }}</p></div>
        </div>
      </section>
      <section class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
        <HostingSettingToggle v-model="flags.auto_reset_credit_enabled" :label="t('admin.accounts.autoResetCredit.title')" :hint="t('admin.accounts.autoResetCredit.hint')" test-id="hosted-auto-reset-credit" />
        <div v-if="flags.auto_reset_credit_enabled" class="grid gap-4 sm:grid-cols-2"><div v-for="window in ['5h', '7d'] as const" :key="window"><label class="input-label">{{ t(`admin.accounts.autoResetCredit.threshold${window}`) }}</label><input v-model.number="resetThresholds[window]" class="input" type="number" min="0" max="100" step="0.1" required :data-testid="`hosted-reset-threshold-${window}`" /></div></div>
        <p v-if="flags.auto_reset_credit_enabled" class="input-hint">{{ t('admin.accounts.autoResetCredit.thresholdHint') }}</p>
      </section>
      <section class="space-y-5 border-t border-gray-200 pt-4 dark:border-dark-600">
        <div><label class="input-label">{{ t('common.status') }}</label><Select v-model="draft.status" :options="statusOptions" :aria-label="t('common.status')" /></div>
        <div v-if="!hidePool">
          <label class="input-label">{{ t('hosting.pool') }}</label>
          <Select v-model="draft.hosting_pool_id" :options="poolOptions" :disabled="disabled || poolDisabled" :aria-label="t('hosting.pool')" data-testid="hosted-pool" />
        </div>
      </section>
    </fieldset>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { buildPlanTypeOptions } from '@/components/account/credentialsBuilder'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import HostingSettingToggle from './HostingSettingToggle.vue'
import { useI18n } from 'vue-i18n'
import AccountRpmSettings from '@/components/account/AccountRpmSettings.vue'
import ModelWhitelistSelector from '@/components/account/ModelWhitelistSelector.vue'
import UpstreamRequestIdHeaderField from '@/components/account/UpstreamRequestIdHeaderField.vue'
import HostingErrorRulesEditor from './HostingErrorRulesEditor.vue'
import { buildHostingErrorRules, hostingErrorRuleDrafts, type HostingErrorRuleDraft } from '@/utils/hostingErrorRules'
import { hostingAPI, type HostedAccountSettings, type HostingPool } from '@/api/hosting'
import { buildModelMappingObject, splitModelMappingObject, getPresetMappingsByPlatform } from '@/composables/useModelWhitelist'
import { oauthModelMappingsError, type OAuthModelMappingRule } from '@/utils/oauthModelMappings'
import { resolveOpenAIWSModeFromExtra, resolveOpenAIWSModeHintKey } from '@/utils/openaiWsMode'
import { useAppStore } from '@/stores/app'

const props = defineProps<{ settings: HostedAccountSettings; accountId?: number; admin?: boolean; pools?: HostingPool[]; disabled?: boolean; poolDisabled?: boolean; hidePool?: boolean }>()
const { t } = useI18n(), app = useAppStore()
const draft = ref<HostedAccountSettings>(), error = ref('')
const planType = ref('')
const planOptions = computed(() => buildPlanTypeOptions(planType.value, t('admin.accounts.openai.planTypeClear')))
const statusOptions = computed(() => [{ value: 'active', label: t('common.active') }, { value: 'inactive', label: t('common.inactive') }, ...(draft.value?.status === 'error' ? [{ value: 'error', label: t('admin.accounts.status.error') }] : []), ...(draft.value?.status === 'disabled' ? [{ value: 'disabled', label: t('common.inactive') }] : [])])
const flags = ref<Record<string, boolean>>({}), enums = ref<Record<string, string>>({})
const modelTab = ref('whitelist')
const presets = getPresetMappingsByPlatform('openai')
const syncModels = computed(() => props.accountId ? () => hostingAPI.syncModels(props.accountId!, props.admin) : undefined)
const aliases = ref(false), allowedModels = ref<string[]>([]), mappings = ref<OAuthModelMappingRule[]>([]), compactMappings = ref<OAuthModelMappingRule[]>([])
const rpmEnabled = ref(false), baseRpm = ref<number | null>(null), imageMode = ref('inherit')
const bpsAllModels = ref(true), bpsModels = ref<string[]>([]), bpsRecoveryMinutes = ref(60)
const expiresAtInput = ref(''), upstreamRequestIdHeader = ref(''), schedulingThreshold = ref<string | number>('')
const tempRulesEnabled = ref(false), tempRules = ref<HostingErrorRuleDraft[]>([])
const resetThresholds = ref({ '5h': 100, '7d': 100 })
const thresholds = ref<Record<'5h' | '7d', string | number>>({ '5h': '', '7d': '' })
const poolOptions = computed(() => [{ value: 0, label: t('hosting.defaultPool') }, ...(props.pools ?? []).filter(pool => pool.id !== 0).map(pool => ({ value: pool.id, label: pool.name }))])
const bpsAvailable = computed(() => app.cachedPublicSettings?.excel_bps_enabled !== false)
const openai = 'admin.accounts.openai.'
const protocolFlags = [
  { key: 'openai_passthrough', label: openai + 'oauthPassthrough', hint: openai + 'oauthPassthroughDesc' },
  { key: 'openai_oauth_ws_sse_acceleration', label: openai + 'wsSseAcceleration', hint: openai + 'wsSseAccelerationDesc' },
  { key: 'openai_responses_flatten_namespaces', label: openai + 'flattenNamespaces', hint: openai + 'flattenNamespacesDesc' },
  { key: 'codex_cli_only', label: openai + 'codexCLIOnly', hint: openai + 'codexCLIOnlyDesc' },
  { key: 'codex_cli_only_allow_app_server', label: openai + 'codexCLIOnlyAppServer', hint: openai + 'codexCLIOnlyAppServerDesc' },
]
const bpsFlags = [
  { key: 'openai_excel_bps_omit_unsupported_tools', label: openai + 'excelBPSOmitUnsupportedTools', hint: openai + 'excelBPSOmitUnsupportedToolsDesc' },
  { key: 'openai_excel_bps_ignore_encrypted_content', label: openai + 'excelBPSIgnoreEncryptedContent', hint: openai + 'excelBPSIgnoreEncryptedContentDesc' },
  { key: 'openai_excel_bps_auto_disable_on_403', label: openai + 'excelBPSAutoDisableOn403', hint: openai + 'excelBPSAutoDisableOn403Desc' },
  { key: 'openai_excel_bps_auto_recover_on_403', label: openai + 'excelBPSAutoRecoverOn403', hint: openai + 'excelBPSAutoRecoverOn403Desc' },
]
const enumFields = computed(() => [
  { key: 'openai_oauth_responses_websockets_v2_mode', label: openai + 'wsMode', hint: resolveOpenAIWSModeHintKey(enums.value.openai_oauth_responses_websockets_v2_mode as 'off' | 'ctx_pool' | 'passthrough' | 'http_bridge') ?? openai + 'wsModeDesc', options: [
    { value: 'off', label: openai + 'wsModeOff' }, { value: 'ctx_pool', label: openai + 'wsModeCtxPool' }, { value: 'passthrough', label: openai + 'wsModePassthrough' }, { value: 'http_bridge', label: openai + 'wsModeHttpBridge' },
  ] },
  { key: 'openai_compact_mode', label: openai + 'compactMode', hint: openai + 'compactModeDesc', options: [
    { value: 'auto', label: openai + 'compactModeAuto' }, { value: 'force_on', label: openai + 'compactModeForceOn' }, { value: 'force_off', label: openai + 'compactModeForceOff' },
  ] },
  { key: 'codex_fingerprint_mode', label: openai + 'codexFingerprintMode', hint: openai + 'codexFingerprintModeDesc', options: [
    { value: 'off', label: openai + 'codexFingerprintOff' }, { value: 'device', label: openai + 'codexFingerprintDevice' }, { value: 'session', label: openai + 'codexFingerprintSession' }, { value: 'full', label: openai + 'codexFingerprintFull' },
  ] },
])
const imageOptions = ['inherit', 'enabled', 'disabled', 'block'].map(value => ({ value, label: openai + 'codexImageTool' + value[0].toUpperCase() + value.slice(1) }))

watch(() => props.settings, settings => {
  error.value = ''
  draft.value = JSON.parse(JSON.stringify(settings)) as HostedAccountSettings
  draft.value.hosting_pool_id ??= 0
  draft.value.status ??= 'active'
  planType.value = String(settings.credentials.plan_type ?? '')
  draft.value.notes ??= ''
  draft.value.auto_pause_on_expired ??= true
  expiresAtInput.value = ''
  if (draft.value.expires_at) {
    const date = new Date(draft.value.expires_at * 1000)
    date.setMinutes(date.getMinutes() - date.getTimezoneOffset())
    expiresAtInput.value = date.toISOString().slice(0, 16)
  }
  const extra = draft.value.extra, credentials = draft.value.credentials
  const modelRules = splitModelMappingObject(credentials.model_mapping as Record<string, unknown>)
  allowedModels.value = modelRules.allowedModels; mappings.value = modelRules.modelMappings
  modelTab.value = mappings.value.length ? 'mapping' : 'whitelist'
  compactMappings.value = Object.entries(credentials.compact_model_mapping as Record<string, string> ?? {}).map(([from, to]) => ({ from, to }))
  aliases.value = credentials.model_mapping_mode === 'aliases'
  flags.value = Object.fromEntries([...protocolFlags, ...bpsFlags, ...['openai_excel_bps', 'auto_pause_5h_disabled', 'auto_pause_7d_disabled', 'auto_reset_credit_enabled'].map(key => ({ key }))].map(field => [field.key, extra[field.key] === true]))
  upstreamRequestIdHeader.value = String(extra.upstream_request_id_header ?? '')
  schedulingThreshold.value = typeof credentials.account_scheduling_threshold === 'number' ? credentials.account_scheduling_threshold : ''
  tempRulesEnabled.value = credentials.temp_unschedulable_enabled === true
  tempRules.value = hostingErrorRuleDrafts(credentials.temp_unschedulable_rules)
  for (const window of ['5h', '7d'] as const) resetThresholds.value[window] = typeof extra[`auto_reset_credit_${window}_threshold`] === 'number' ? Number(extra[`auto_reset_credit_${window}_threshold`]) * 100 : 100
  enums.value = {
    openai_oauth_responses_websockets_v2_mode: resolveOpenAIWSModeFromExtra(extra, { modeKey: 'openai_oauth_responses_websockets_v2_mode', enabledKey: 'openai_oauth_responses_websockets_v2_enabled' }),
    openai_compact_mode: String(extra.openai_compact_mode ?? 'auto'), codex_fingerprint_mode: String(extra.codex_fingerprint_mode ?? 'off'),
  }
  baseRpm.value = Number(extra.base_rpm) || null; rpmEnabled.value = (baseRpm.value ?? 0) > 0
  imageMode.value = extra.codex_image_generation_explicit_tool_policy === 'strip' ? 'block' : typeof extra.codex_image_generation_bridge === 'boolean' ? extra.codex_image_generation_bridge ? 'enabled' : 'disabled' : 'inherit'
  bpsAllModels.value = extra.openai_excel_bps_models == null; bpsModels.value = (extra.openai_excel_bps_models as string[] | undefined) ?? []
  bpsRecoveryMinutes.value = Number(extra.openai_excel_bps_403_recovery_interval_minutes) || 60
  for (const window of ['5h', '7d'] as const) thresholds.value[window] = typeof extra[`auto_pause_${window}_threshold`] === 'number' ? Number(extra[`auto_pause_${window}_threshold`]) * 100 : ''
}, { immediate: true })

function addPreset(from: string, to: string) { if (!mappings.value.some(rule => rule.from === from)) mappings.value.push({ from, to }) }
function buildSettings(): HostedAccountSettings | undefined {
  if (!draft.value || props.disabled) return
  error.value = ''
  const errorRules = tempRulesEnabled.value ? buildHostingErrorRules(tempRules.value) : null
  if (tempRulesEnabled.value && !errorRules) { error.value = t('admin.accounts.tempUnschedulable.rulesInvalid'); return }
  if (flags.value.auto_reset_credit_enabled && Object.values(resetThresholds.value).some(value => value !== 0 && (!Number.isFinite(value) || value < .1 || value > 100))) { error.value = t('admin.accounts.autoResetCredit.thresholdInvalid'); return }
  const mappingError = (!flags.value.openai_passthrough && oauthModelMappingsError(mappings.value)) || oauthModelMappingsError(compactMappings.value)
  if (mappingError) { error.value = t(mappingError); return }
  if (bpsAvailable.value && flags.value.openai_excel_bps && !bpsAllModels.value && !bpsModels.value.length) { error.value = t('autoConfig.bps.modelsRequired'); return }
  const credentials: Record<string, unknown> = { ...draft.value.credentials, model_mapping_mode: aliases.value ? 'aliases' : 'whitelist', compact_model_mapping: buildModelMappingObject('mapping', [], compactMappings.value) }
  if (!flags.value.openai_passthrough) credentials.model_mapping = buildModelMappingObject('combined', allowedModels.value, mappings.value)
  if (planType.value || 'plan_type' in draft.value.credentials) credentials.plan_type = planType.value || null
  credentials.account_scheduling_threshold = schedulingThreshold.value === '' ? null : Number(schedulingThreshold.value)
  credentials.temp_unschedulable_enabled = tempRulesEnabled.value
  credentials.temp_unschedulable_rules = errorRules
  const extra: Record<string, unknown> = { ...draft.value.extra, ...enums.value, ...flags.value, base_rpm: rpmEnabled.value ? baseRpm.value ?? 15 : null }
  extra.upstream_request_id_header = upstreamRequestIdHeader.value.trim() || null
  for (const key of ['openai_prism_browser', 'openai_prism_browser_models', 'openai_excel_bps_mihomo', 'openai_excel_bps_proxy_source']) delete extra[key]
  extra.codex_image_generation_bridge = ['enabled', 'disabled'].includes(imageMode.value) ? imageMode.value === 'enabled' : null
  extra.codex_image_generation_explicit_tool_policy = imageMode.value === 'block' ? 'strip' : null
  if (bpsAvailable.value) {
    extra.openai_excel_bps_config_mode = draft.value.extra.openai_excel_bps_config_mode ?? 'initial'
    extra.openai_excel_bps_models = bpsAllModels.value ? null : bpsModels.value
    extra.openai_excel_bps_auto_recover_on_403 = flags.value.openai_excel_bps_auto_disable_on_403 && flags.value.openai_excel_bps_auto_recover_on_403
    extra.openai_excel_bps_403_recovery_interval_minutes = bpsRecoveryMinutes.value
  } else {
    for (const key of Object.keys(extra)) if (key.startsWith('openai_excel_bps')) delete extra[key]
  }
  for (const window of ['5h', '7d'] as const) extra[`auto_pause_${window}_threshold`] = thresholds.value[window] === '' ? null : Number(thresholds.value[window]) / 100
  for (const window of ['5h', '7d'] as const) extra[`auto_reset_credit_${window}_threshold`] = resetThresholds.value[window] / 100
  return { ...draft.value, status: draft.value.status === (props.settings.status ?? 'active') ? undefined : draft.value.status, load_factor: Number(draft.value.load_factor) || 0, expires_at: expiresAtInput.value ? Math.floor(new Date(expiresAtInput.value).getTime() / 1000) : 0, hosting_pool_id: draft.value.hosting_pool_id ?? 0, credentials, extra }
}
defineExpose({ buildSettings })
</script>
