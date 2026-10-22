import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

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

// Render the scheduler-score cell slot for every row so the fallback logic is observable.
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
describe('account manual scheduling warnings', () => {
  it('leaves the immediate switch untouched when cancellation is declined', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="scheduling-7"] button').trigger('click')
    expect(confirm).toHaveBeenCalledWith('admin.accounts.timer.manualWarning')
    expect(setSchedulable).not.toHaveBeenCalled()
  })
  it('confirms the immediate switch and refreshes plans after its atomic backend update', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="scheduling-7"] button').trigger('click')
    await flushPromises()
    expect(setSchedulable).toHaveBeenCalledWith(7, false)
    expect(wrapper.findComponent({ name: 'SchedulableTimerControl' }).props('refreshKey')).toBe(1)
  })
  it('also warns before batch immediate switching', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    wrapper = mountView()
    await flushPromises()
    wrapper.findComponent({ name: 'AccountBulkActionsBar' }).vm.$emit('toggle-schedulable', false)
    await flushPromises()
    expect(confirm).toHaveBeenCalledWith('admin.accounts.timer.manualWarning')
    expect(bulkUpdate).not.toHaveBeenCalled()
  })
})
