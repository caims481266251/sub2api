import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import zh from '@/i18n/locales/zh'
import type { HostedAccount } from '@/api/hosting'
import HostingAccountSettingsDialog from '../HostingAccountSettingsDialog.vue'
import HostingErrorRulesEditor from '../HostingErrorRulesEditor.vue'

const { saveAccountSettings, app } = vi.hoisted(() => ({
  saveAccountSettings: vi.fn(),
  app: { showSuccess: vi.fn(), cachedPublicSettings: { excel_bps_enabled: true, prism_browser_enabled: true } },
}))
vi.mock('@/api/hosting', async importOriginal => ({ ...await importOriginal<typeof import('@/api/hosting')>(), hostingAPI: { saveAccountSettings } }))
vi.mock('@/components/common/Select.vue', () => ({ default: {
  props: ['modelValue', 'options', 'disabled'], emits: ['update:modelValue'],
  template: `<select :value="modelValue" :disabled="disabled" @change="$emit('update:modelValue', Number.isFinite(Number($event.target.value)) ? Number($event.target.value) : $event.target.value)"><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>`,
} }))
vi.mock('@/stores/app', () => ({ useAppStore: () => app }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => {
    let value: unknown = zh
    for (const part of key.split('.')) value = value && typeof value === 'object' ? (value as Record<string, unknown>)[part] : undefined
    if (typeof value !== 'string') throw new Error(`Missing translation: ${key}`)
    return value
  } }),
}))

const account = (): HostedAccount => ({
  id: 17, name: 'Codex', email: 'user@example.com', state: 'active', reset_at: null, last_used_at: null, usage_5h_percent: null, usage_7d_percent: null,
  settings: { hosting_pool_id: null, name: 'Codex', concurrency: 3, priority: 50, credentials: { model_mapping: { 'client-model': 'gpt-5.4' } }, extra: {} },
})
const render = (value = account()) => mount(HostingAccountSettingsDialog, {
  props: { account: reactive(value) },
  global: {
    stubs: {
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot/><slot name="footer"/></div>' },
      ModelWhitelistSelector: true,
    },
  },
})

describe('Hosted OpenAI account settings', () => {
  it('edits status and plan while omitting an unchanged runtime status', async () => {
    const value = account(); value.settings.status = 'error'; value.settings.credentials.plan_type = 'plus'
    const wrapper = render(value)
    await wrapper.get('#hosted-name').setValue('Renamed')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings.mock.calls[0][1].status).toBeUndefined()
    expect(saveAccountSettings.mock.calls[0][1].credentials.plan_type).toBe('plus')
    await wrapper.get('[aria-label="状态"]').setValue('inactive')
    await wrapper.get('[aria-label="' + zh.admin.accounts.openai.planType + '"]').setValue('pro')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings.mock.calls[1][1]).toMatchObject({ status: 'inactive', credentials: { plan_type: 'pro' } })
    wrapper.unmount()
  })
  it('moves an account between owned pools and explicitly clears to the default pool', async () => {
    const value = account(); value.settings.hosting_pool_id = 31
    const wrapper = render(value)
    await wrapper.setProps({ pools: [{ id: 31, name: '工作池', account_count: 1, key_count: 1 }] })
    expect(wrapper.get<HTMLSelectElement>('[data-testid="hosted-pool"]').element.value).toBe('31')
    await wrapper.get('[data-testid="hosted-pool"]').setValue('0')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings).toHaveBeenCalledWith(17, expect.objectContaining({ hosting_pool_id: 0 }))
    expect(value.settings.hosting_pool_id).toBe(31)
    wrapper.unmount()
  })
  beforeEach(() => { saveAccountSettings.mockReset(); saveAccountSettings.mockResolvedValue({}); app.showSuccess.mockReset(); app.cachedPublicSettings.excel_bps_enabled = true; app.cachedPublicSettings.prism_browser_enabled = true })
  it('saves existing OpenAI fields and WS selection while keeping the source account intact', async () => {
    const original = account()
    const wrapper = render(original)
    expect(wrapper.text()).toContain('自动透传')
    expect(wrapper.text()).not.toContain('admin.accounts.')
    await wrapper.get('[data-testid="hosted-model-aliases"]').trigger('click')
    await wrapper.get('[data-testid="hosted-bps-enabled"]').trigger('click')
    await wrapper.get('[data-testid="hosted-openai_oauth_responses_websockets_v2_mode"]').setValue('http_bridge')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings).toHaveBeenCalledWith(17, expect.objectContaining({
      credentials: expect.objectContaining({ model_mapping_mode: 'aliases', model_mapping: { 'client-model': 'gpt-5.4' }, compact_model_mapping: null }),
      extra: expect.objectContaining({ openai_oauth_responses_websockets_v2_mode: 'http_bridge', openai_compact_mode: 'auto', base_rpm: null }),
    }))
    expect(original.settings.credentials.model_mapping_mode).toBeUndefined()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    wrapper.unmount()
  })
  it('preserves saved model mappings while passthrough is enabled', async () => {
    const value = account(); value.settings.extra.openai_passthrough = true
    const wrapper = render(value)
    expect(wrapper.text()).toContain('关闭透传后')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings.mock.calls[0][1].credentials.model_mapping).toEqual(value.settings.credentials.model_mapping)
    wrapper.unmount()
  })
  it('clears removed mappings with null so server patching actually resets them', async () => {
    const wrapper = render()
    await wrapper.get('[aria-label="删除"]').trigger('click')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings.mock.calls[0][1].credentials.model_mapping).toBeNull()
    wrapper.unmount()
  })
  it('honors the global BPS switch and displays server errors without closing', async () => {
    app.cachedPublicSettings.excel_bps_enabled = false
    saveAccountSettings.mockRejectedValue({ message: '账号不属于当前用户' })
    const wrapper = render()
    expect(wrapper.find('[data-testid="hosted-bps-enabled"]').exists()).toBe(false)
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('账号不属于当前用户')
    expect(Object.keys(saveAccountSettings.mock.calls[0][1].extra).some(key => key.startsWith('openai_excel_bps'))).toBe(false)
    expect(wrapper.emitted('close')).toBeUndefined()
    wrapper.unmount()
  })

  it('saves advanced OpenAI calling preferences using the shared request ID control', async () => {
    const original = account()
    const wrapper = render(original)
    await wrapper.get('[data-testid="hosted-notes"]').setValue('自用配置')
    await wrapper.get('[data-testid="hosted-load-factor"]').setValue(8)
    await wrapper.get('[data-testid="hosted-expires-at"]').setValue('2030-01-02T03:04')
    await wrapper.get('[data-testid="hosted-scheduling-threshold"]').setValue(85)
    await wrapper.get('[data-testid="hosted-bps-enabled"]').trigger('click')
    await wrapper.get('[data-testid="upstream-request-id-header"]').setValue(' x-request-id ')
    await wrapper.get('[data-testid="hosted-auto-reset-credit"]').trigger('click')
    await wrapper.get('[data-testid="hosted-reset-threshold-5h"]').setValue(0)
    await wrapper.get('[data-testid="hosted-reset-threshold-7d"]').setValue(90)
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings).toHaveBeenCalledWith(17, expect.objectContaining({
      notes: '自用配置', load_factor: 8, expires_at: Math.floor(new Date('2030-01-02T03:04').getTime() / 1000), auto_pause_on_expired: true,
      credentials: expect.objectContaining({ account_scheduling_threshold: 85 }),
      extra: expect.objectContaining({ upstream_request_id_header: 'x-request-id', auto_reset_credit_enabled: true, auto_reset_credit_5h_threshold: 0, auto_reset_credit_7d_threshold: .9 }),
    }))
    expect(original.settings.extra).toEqual({})
    wrapper.unmount()
  })

  it('explicitly clears saved expiry, load factor and inherited settings', async () => {
    const value = account()
    Object.assign(value.settings, { load_factor: 8, expires_at: 2000000000 })
    value.settings.credentials.account_scheduling_threshold = 85
    value.settings.extra.upstream_request_id_header = 'x-id'
    const wrapper = render(value)
    await wrapper.get('[data-testid="hosted-load-factor"]').setValue('')
    await wrapper.get('[data-testid="hosted-expires-at"]').setValue('')
    await wrapper.get('[data-testid="hosted-scheduling-threshold"]').setValue('')
    await wrapper.get('[data-testid="upstream-request-id-header"]').setValue('')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings.mock.calls[0][1]).toMatchObject({ load_factor: 0, expires_at: 0, credentials: { account_scheduling_threshold: null }, extra: { upstream_request_id_header: null } })
    wrapper.unmount()
  })

  it('excludes Prism and BPS network preferences even when globally enabled', async () => {
    const value = account()
    value.settings.extra = { openai_excel_bps: true, openai_excel_bps_mihomo: true, openai_excel_bps_proxy_source: 'ip_pool', openai_prism_browser: true, openai_prism_browser_models: ['gpt-6.1-sol'] }
    const wrapper = render(value)
    expect(wrapper.find('[data-testid="hosted-bps-enabled"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="openai-prism-browser-oauth-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="hosted-bps-network"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Prism')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    const saved = saveAccountSettings.mock.calls[0][1].extra
    expect(Object.keys(saved).filter(key => key.startsWith('openai_prism_browser') || ['openai_excel_bps_mihomo', 'openai_excel_bps_proxy_source'].includes(key))).toEqual([])
    expect(value.settings.extra.openai_excel_bps).toBe(true)
    wrapper.unmount()
  })

  it('rejects incomplete error rules, then saves ordered rules and normalized keywords', async () => {
    const wrapper = render()
    await wrapper.get('[data-testid="hosted-error-rules-enabled"]').trigger('click')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    expect(saveAccountSettings).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('规则')
    await wrapper.get('[data-testid="hosted-add-error-rule"]').trigger('click')
    await wrapper.get('[data-testid="hosted-add-error-rule"]').trigger('click')
    const editor = wrapper.getComponent(HostingErrorRulesEditor)
    const first = editor.findAll('fieldset')[0].findAll('input')
    await first[2].setValue(' limit，rate， busy ')
    await first[3].setValue('  限额  ')
    const second = editor.findAll('fieldset')[1].findAll('input')
    await second[0].setValue(503)
    await second[1].setValue(5)
    await second[2].setValue('overloaded')
    await wrapper.findAll('[aria-label="规则上移"]')[1].trigger('click')
    await wrapper.get('#hosting-settings-form').trigger('submit')
    await flushPromises()
    expect(saveAccountSettings.mock.calls[0][1].credentials.temp_unschedulable_rules).toEqual([
      { error_code: 503, keywords: ['overloaded'], duration_minutes: 5, description: '' },
      { error_code: 429, keywords: ['limit', 'rate', 'busy'], duration_minutes: 30, description: '限额' },
    ])
    wrapper.unmount()
  })

  it('keeps automatic reset credits off by default and rejects a fractional threshold below 0.1 percent', async () => {
    const wrapper = render()
    expect(wrapper.get('[data-testid="hosted-auto-reset-credit"]').attributes('aria-checked')).toBe('false')
    await wrapper.get('[data-testid="hosted-auto-reset-credit"]').trigger('click')
    await wrapper.get('[data-testid="hosted-reset-threshold-5h"]').setValue(.01)
    await wrapper.get('#hosting-settings-form').trigger('submit')
    expect(saveAccountSettings).not.toHaveBeenCalled()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    wrapper.unmount()
  })
})
