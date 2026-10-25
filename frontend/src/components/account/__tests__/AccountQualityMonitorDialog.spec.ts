import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import AccountQualityMonitorDialog from '../AccountQualityMonitorDialog.vue'
import { getAccountQuality, runAccountQuality, saveAccountQuality, type AccountQualityMonitor } from '@/api/admin/accountActivity'
import { getAvailableModels } from '@/api/admin/accounts'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('@/api/admin/accountActivity', () => ({ getAccountQuality: vi.fn(), runAccountQuality: vi.fn(), saveAccountQuality: vi.fn() }))
vi.mock('@/api/admin/accounts', () => ({ getAvailableModels: vi.fn() }))

function quality(overrides: Partial<AccountQualityMonitor> = {}): AccountQualityMonitor {
  return { account_id: 1, scope: 'scope-1', enabled: false, model: 'gpt-5', interval_minutes: 5, state: 'untested', ...overrides }
}
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}
const modelStub = {
  props: ['modelValue', 'options'], emits: ['update:modelValue'],
  template: '<div><input data-testid="model-input" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" /><span data-testid="model-options">{{ options }}</span></div>'
}

describe('AccountQualityMonitorDialog', () => {
  let wrapper: VueWrapper
  function render(show = true) {
    wrapper = mount(AccountQualityMonitorDialog, {
      props: { show, account: { id: 1, name: 'OpenAI 1' } },
      global: { stubs: {
        BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
        TestModelSelect: modelStub
      } }
    })
    return wrapper
  }
  beforeEach(() => {
    vi.mocked(getAccountQuality).mockReset().mockResolvedValue(quality())
    vi.mocked(getAvailableModels).mockReset().mockResolvedValue([{ id: 'gpt-5', display_name: 'GPT-5', type: 'model', created_at: '' }])
    vi.mocked(runAccountQuality).mockReset().mockResolvedValue(quality({ state: 'running' }))
    vi.mocked(saveAccountQuality).mockReset()
  })
  afterEach(() => { wrapper?.unmount() })

  it('omits known non-text models from this picker without matching incidental words in provider aliases', async () => {
    const excluded = ['gpt-image-1', 'chatgpt-image-latest', 'dall-e-3', 'gpt-4o-audio-preview', 'gpt-4o-mini-transcribe', 'gpt-realtime-mini', 'whisper-1', 'tts-1', 'text-embedding-3-small', 'omni-moderation-latest', 'sora-2']
    const included = ['gpt-5', 'vendor-image-aware-chat', 'acme-embedding-explainer']
    vi.mocked(getAvailableModels).mockResolvedValue([...excluded, ...included].map(id => ({ id, display_name: id, type: 'model', created_at: '' })))
    render()
    await flushPromises()
    const options = wrapper.get('[data-testid="model-options"]').text()
    for (const id of excluded) expect(options).not.toContain(id)
    for (const id of included) expect(options).toContain(id)
    expect(getAvailableModels).toHaveBeenCalledWith(1)
  })

  it('allows a manual check with automatic monitoring off and sends only the current identity scope', async () => {
    render()
    await flushPromises()
    expect(wrapper.get('[data-testid="run-quality"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[data-testid="run-quality"]').trigger('click')
    await flushPromises()
    expect(runAccountQuality).toHaveBeenCalledWith(1, 'scope-1')
    expect(saveAccountQuality).not.toHaveBeenCalled()
    expect((wrapper.get('[data-testid="quality-auto-enabled"]').element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.get('[data-testid="quality-state"]').text()).toContain('running')
  })

  it('keeps edited settings after a failed save and requires saving before a manual run', async () => {
    vi.mocked(saveAccountQuality).mockRejectedValue(new Error('save unavailable'))
    render()
    await flushPromises()
    await wrapper.get('[data-testid="model-input"]').setValue('gpt-new')
    await wrapper.get('[data-testid="quality-interval"]').setValue('12')
    expect(wrapper.get('[data-testid="run-quality"]').attributes('disabled')).toBeDefined()
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(saveAccountQuality).toHaveBeenCalledWith(1, { scope: 'scope-1', enabled: false, model: 'gpt-new', interval_minutes: 12 })
    expect((wrapper.get('[data-testid="model-input"]').element as HTMLInputElement).value).toBe('gpt-new')
    expect((wrapper.get('[data-testid="quality-interval"]').element as HTMLInputElement).value).toBe('12')
    expect(wrapper.get('[data-testid="save-quality"]').attributes('disabled')).toBeUndefined()
  })

  it('shows the last valid judgement alongside a later request error', async () => {
    vi.mocked(getAccountQuality).mockResolvedValue(quality({
      state: 'error', latest_attempt: { state: 'error', model: 'gpt-5', checked_at: '2026-10-25T02:00:00Z', duration_ms: 3000, error: 'upstream timeout' },
      last_valid: { state: 'passed', answer: '21', model: 'gpt-5', checked_at: '2026-10-25T01:00:00Z', duration_ms: 1000 }
    }))
    render()
    await flushPromises()
    expect(wrapper.get('[data-testid="quality-state"]').text()).toContain('error')
    expect(wrapper.get('[data-testid="quality-last-valid"]').text()).toContain('passed')
    expect(wrapper.get('[data-testid="quality-last-valid"]').text()).toContain('21')
    expect(wrapper.text()).toContain('upstream timeout')
  })

  it('ignores late detail and model replies for a previously selected account', async () => {
    const oldDetail = deferred<AccountQualityMonitor>()
    const oldModels = deferred<Awaited<ReturnType<typeof getAvailableModels>>>()
    vi.mocked(getAccountQuality).mockReturnValueOnce(oldDetail.promise).mockResolvedValueOnce(quality({ account_id: 2, scope: 'scope-2', model: 'new-model' }))
    vi.mocked(getAvailableModels).mockReturnValueOnce(oldModels.promise).mockResolvedValueOnce([{ id: 'new-model', display_name: 'New', type: 'model', created_at: '' }])
    render()
    const oldSignal = vi.mocked(getAccountQuality).mock.calls[0]![1]!
    await wrapper.setProps({ account: { id: 2, name: 'OpenAI 2' } })
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    oldDetail.resolve(quality({ model: 'old-model' }))
    oldModels.resolve([{ id: 'old-model', display_name: 'Old', type: 'model', created_at: '' }])
    await flushPromises()
    expect((wrapper.get('[data-testid="model-input"]').element as HTMLInputElement).value).toBe('new-model')
    expect(wrapper.get('[data-testid="model-options"]').text()).not.toContain('old-model')
  })

  it('loads original answer text when the list result changes and preserves unsaved settings', async () => {
    render()
    await flushPromises()
    await wrapper.get('[data-testid="quality-interval"]').setValue('12')
    const completed = quality({ state: 'passed', latest_attempt: { state: 'passed', answer: '21', model: 'gpt-5', checked_at: '2026-10-25T01:00:00Z', duration_ms: 1000 } })
    vi.mocked(getAccountQuality).mockResolvedValueOnce({ ...completed, latest_attempt: { ...completed.latest_attempt!, text: 'The final answer is 21.' } })
    await wrapper.setProps({ summary: completed })
    await flushPromises()
    expect(getAccountQuality).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-testid="quality-raw-answer"]').text()).toContain('The final answer is 21.')
    expect((wrapper.get('[data-testid="quality-interval"]').element as HTMLInputElement).value).toBe('12')
    await wrapper.setProps({ summary: { ...completed } })
    await flushPromises()
    expect(getAccountQuality).toHaveBeenCalledTimes(2)
  })

  it('resets a draft when the same account changes identity and sends the new scope', async () => {
    render()
    await flushPromises()
    await wrapper.get('[data-testid="model-input"]').setValue('unsaved-old-model')
    const changed = quality({ scope: 'scope-new', model: 'new-model' })
    vi.mocked(getAccountQuality).mockResolvedValueOnce(changed)
    await wrapper.setProps({ summary: changed })
    await flushPromises()
    expect((wrapper.get('[data-testid="model-input"]').element as HTMLInputElement).value).toBe('new-model')
    expect(wrapper.text()).toContain('admin.accounts.activity.accountChanged')
    await wrapper.get('[data-testid="run-quality"]').trigger('click')
    expect(runAccountQuality).toHaveBeenCalledWith(1, 'scope-new')
  })

  it('does not request detail while the dialog is closed', async () => {
    render(false)
    await wrapper.setProps({ summary: quality({ state: 'running' }) })
    await flushPromises()
    expect(getAccountQuality).not.toHaveBeenCalled()
    expect(getAvailableModels).not.toHaveBeenCalled()
  })
})
