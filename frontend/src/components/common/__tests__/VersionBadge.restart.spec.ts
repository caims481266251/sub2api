import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { checkUpdates, getRollbackVersions, performUpdate, restartService, rollback, type VersionInfo } from '@/api/admin/system'
import messages from '@/i18n/locales/en/misc'
import VersionBadge from '../VersionBadge.vue'

vi.mock('@/stores', async () => {
  const { useAppStore } = await import('@/stores/app')
  return { useAppStore, useAuthStore: () => ({ isAdmin: true }) }
})

vi.mock('@/api/admin/system', () => ({
  checkUpdates: vi.fn(),
  getRollbackVersions: vi.fn(),
  performUpdate: vi.fn(),
  restartService: vi.fn(),
  rollback: vi.fn(),
}))

const versionInfo: VersionInfo = {
  current_version: '2.15.15', latest_version: '2.15.16', has_update: true,
  cached: false, build_type: 'release',
}
const wrappers: VueWrapper[] = []

async function renderBadge() {
  const wrapper = mount(VersionBadge, {
    global: { plugins: [createI18n({ legacy: false, locale: 'en', messages: { en: messages } })] },
  })
  wrappers.push(wrapper)
  await flushPromises()
  await wrapper.get('button').trigger('click')
  return wrapper
}

function button(wrapper: VueWrapper, label: string) {
  const found = wrapper.findAll('button').find(item => item.text() === label)
  expect(found, 'missing button ' + label).toBeDefined()
  return found!
}

function expectPendingRestart(wrapper: VueWrapper) {
  expect(button(wrapper, 'Restart Now').exists()).toBe(true)
  expect(wrapper.text()).toContain(messages.version.restartRequired)
  expect(wrapper.text()).not.toContain('Update Failed')
  expect(wrapper.findAll('button').some(item => ['Update Now', 'Retry', 'Version Rollback'].includes(item.text()))).toBe(false)
  expect(restartService).not.toHaveBeenCalled()
}

beforeEach(() => {
  vi.clearAllMocks()
  setActivePinia(createPinia())
  vi.mocked(checkUpdates).mockResolvedValue(versionInfo)
  vi.mocked(getRollbackVersions).mockResolvedValue({
    versions: [{ version: '2.15.14', published_at: '', html_url: '#' }],
  })
})

afterEach(() => {
  for (const wrapper of wrappers.splice(0)) wrapper.unmount()
})

describe('VersionBadge pending restart', () => {
  it('restores pending restart from the server on a fresh mount', async () => {
    vi.mocked(checkUpdates).mockResolvedValue({ ...versionInfo, restart_required: true })
    const wrapper = await renderBadge()
    expectPendingRestart(wrapper)
    expect(performUpdate).not.toHaveBeenCalled()
  })

  it('keeps a successful update pending after refresh and component remount with an older server', async () => {
    vi.mocked(performUpdate).mockResolvedValue({ message: 'installed', need_restart: true })
    const wrapper = await renderBadge()
    await button(wrapper, 'Update Now').trigger('click')
    await flushPromises()
    expectPendingRestart(wrapper)
    expect(wrapper.text()).toContain('Update Complete')
    await wrapper.get('button[title="Refresh"]').trigger('click')
    await flushPromises()
    expectPendingRestart(wrapper)
    wrapper.unmount()
    const remounted = await renderBadge()
    expectPendingRestart(remounted)
    expect(performUpdate).toHaveBeenCalledTimes(1)
  })

  it.each([
    { reason: 'SYSTEM_RESTART_REQUIRED', message: 'restart first' },
    { response: { data: { reason: 'SYSTEM_RESTART_REQUIRED', message: 'restart first' } } },
  ])('turns an update restart conflict into the restart panel: %j', async error => {
    vi.mocked(performUpdate).mockRejectedValue(error)
    const wrapper = await renderBadge()
    await button(wrapper, 'Update Now').trigger('click')
    await flushPromises()
    expectPendingRestart(wrapper)
    expect(useAppStore().restartRequired).toBe(true)
  })

  it('keeps a genuine update failure visible and retryable', async () => {
    vi.mocked(performUpdate).mockRejectedValue({ reason: 'DOWNLOAD_FAILED', message: 'download unavailable' })
    const wrapper = await renderBadge()
    await button(wrapper, 'Update Now').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Update Failed')
    expect(wrapper.text()).toContain('download unavailable')
    expect(button(wrapper, 'Retry').exists()).toBe(true)
    expect(useAppStore().restartRequired).toBe(false)
  })

  it.each(['success', 'conflict'])('handles rollback %s through the same pending state', async outcome => {
    vi.mocked(checkUpdates).mockResolvedValue({ ...versionInfo, latest_version: '2.15.15', has_update: false })
    if (outcome === 'success') {
      vi.mocked(rollback).mockResolvedValue({ message: 'installed', need_restart: true })
    } else {
      vi.mocked(rollback).mockRejectedValue({ reason: 'SYSTEM_RESTART_REQUIRED', message: 'restart first' })
    }
    const wrapper = await renderBadge()
    await button(wrapper, 'Version Rollback').trigger('click')
    await flushPromises()
    const target = wrapper.findAll('button').find(item => item.text().includes('v2.15.14'))
    expect(target).toBeDefined()
    await target!.trigger('click')
    await button(wrapper, 'Roll back to v2.15.14').trigger('click')
    await flushPromises()
    expectPendingRestart(wrapper)
    expect(rollback).toHaveBeenCalledWith('2.15.14')
    expect(wrapper.text()).not.toContain('Rollback Failed')
    if (outcome === 'success') expect(wrapper.text()).toContain('Rollback Complete')
  })

  it('clears pending UI after the server reports its new running version', async () => {
    vi.mocked(checkUpdates).mockResolvedValue({ ...versionInfo, restart_required: true })
    const wrapper = await renderBadge()
    expectPendingRestart(wrapper)
    vi.mocked(checkUpdates).mockResolvedValue({
      ...versionInfo, current_version: '2.15.16', has_update: false, restart_required: false,
    })
    await wrapper.get('button[title="Refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('button').some(item => item.text() === 'Restart Now')).toBe(false)
    expect(wrapper.text()).toContain('v2.15.16')
  })
})
