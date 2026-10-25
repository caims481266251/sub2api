import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

vi.mock('@/api/admin/accountActivity', () => ({ getAccountActivity: vi.fn().mockResolvedValue([]) }))

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getAllProxies,
  getAllGroups,
  setSchedulable,
  bulkUpdate,
  getProbeSettings,
  getCapabilities
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  setSchedulable: vi.fn(),
  bulkUpdate: vi.fn(),
  getProbeSettings: vi.fn(),
  getCapabilities: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getManagementCapabilities: getCapabilities,
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings: getProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      setSchedulable,
      bulkUpdate
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token', isAdmin: true
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

// Render each scheduling cell so the immediate switch and timer refresh are observable.
const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <div data-test="data-table">
      <div v-for="row in data" :key="row.id" :data-test="'scheduling-' + row.id">
        <slot name="cell-schedulable" :row="row" />
      </div>
    </div>
  `
}

function mountView() {
  return mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        DataTable: DataTableStub,
        HelpTooltip: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
        AccountTableFilters: { template: '<div></div>' },
        AccountBulkActionsBar: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        SchedulableTimerControl: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        Icon: true
      }
    }
  })
}

const baseAccount = {
  platform: 'openai',
  type: 'apikey',
  status: 'active',
  schedulable: true,
  concurrency: 1,
  priority: 0,
  error_message: null,
  last_used_at: null,
  expires_at: null,
  auto_pause_on_expired: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
}


let wrapper: ReturnType<typeof mountView>
beforeEach(() => {
  vi.clearAllMocks()
  getProbeSettings.mockResolvedValue({ enabled: true, interval_minutes: 30 })
  getCapabilities.mockResolvedValue({ web_search_enabled: false, account_quota_notify_enabled: false })
  localStorage.clear()
  listAccounts.mockResolvedValue({ items: [{ ...baseAccount, id: 7, name: 'timer-fixture' }], total: 1, page: 1, page_size: 20, pages: 1 })
  listWithEtag.mockResolvedValue({ notModified: true, etag: null, data: null })
  getBatchTodayStats.mockResolvedValue({ stats: {} })
  getAllProxies.mockResolvedValue([])
  getAllGroups.mockResolvedValue([])
  setSchedulable.mockResolvedValue({ ...baseAccount, id: 7, schedulable: false })
  bulkUpdate.mockResolvedValue({ success: 1, failed: 0, success_ids: [7], failed_ids: [] })
})
afterEach(() => { wrapper?.unmount(); vi.restoreAllMocks() })
describe('account immediate scheduling', () => {
  it.each([true, false])('toggles a currently %s account without confirmation and refreshes plans', async (schedulable) => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    listAccounts.mockResolvedValue({ items: [{ ...baseAccount, id: 7, name: 'timer-fixture', schedulable }], total: 1, page: 1, page_size: 20, pages: 1 })
    setSchedulable.mockResolvedValue({ ...baseAccount, id: 7, schedulable: !schedulable })
    wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="scheduling-7"] button').trigger('click')
    await flushPromises()
    expect(confirm).not.toHaveBeenCalled()
    expect(setSchedulable).toHaveBeenCalledTimes(1)
    expect(setSchedulable).toHaveBeenCalledWith(7, !schedulable)
    expect(wrapper.get('[data-test="scheduling-7"] button').attributes('aria-checked')).toBe(String(!schedulable))
    expect(wrapper.findComponent({ name: 'SchedulableTimerControl' }).props('refreshKey')).toBe(1)
  })
  it.each([true, false])('sets bulk scheduling to %s without confirmation and refreshes plans', async (schedulable) => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    wrapper = mountView()
    await flushPromises()
    const bulkActions = wrapper.findComponent({ name: 'AccountBulkActionsBar' })
    bulkActions.vm.$emit('select-page')
    await flushPromises()
    bulkActions.vm.$emit('toggle-schedulable', schedulable)
    await flushPromises()
    expect(confirm).not.toHaveBeenCalled()
    expect(bulkUpdate).toHaveBeenCalledTimes(1)
    expect(bulkUpdate).toHaveBeenCalledWith([7], { schedulable })
    expect(wrapper.get('[data-test="scheduling-7"] button').attributes('aria-checked')).toBe(String(schedulable))
    expect(wrapper.findComponent({ name: 'SchedulableTimerControl' }).props('refreshKey')).toBe(1)
  })
})
