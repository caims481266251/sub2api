import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, nextTick, ref } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { getAccountActivity, type AccountActivity } from '@/api/admin/accountActivity'
import { useAccountActivity } from '@/composables/useAccountActivity'

vi.mock('@/api/admin/accountActivity', () => ({ getAccountActivity: vi.fn() }))
const fetchActivity = vi.mocked(getAccountActivity)
const activity = (id: number): AccountActivity => ({
  account_id: id,
  quality: { account_id: id, scope: `scope-${id}`, enabled: false, model: 'gpt-5', interval_minutes: 5, state: 'untested' },
  recent_requests: []
})

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

describe('useAccountActivity', () => {
  let wrapper: VueWrapper | undefined
  let hidden = false
  function setup(rows = [{ id: 1, platform: 'openai' }]) {
    const accounts = ref(rows)
    let state!: ReturnType<typeof useAccountActivity>
    wrapper = mount(defineComponent({ setup() { state = useAccountActivity(accounts); return () => null } }))
    return { accounts, state }
  }

  beforeEach(() => {
    vi.useFakeTimers()
    hidden = false
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden)
    fetchActivity.mockReset().mockResolvedValue([])
  })
  afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('batches only visible OpenAI rows and prevents overlapping polls', async () => {
    const pending = deferred<AccountActivity[]>()
    fetchActivity.mockReturnValueOnce(pending.promise)
    const { state } = setup([{ id: 1, platform: 'openai' }, { id: 2, platform: 'anthropic' }, { id: 3, platform: 'openai' }])
    expect(fetchActivity).toHaveBeenCalledWith([1, 3], expect.any(AbortSignal))
    await vi.advanceTimersByTimeAsync(15000)
    expect(fetchActivity).toHaveBeenCalledTimes(1)
    pending.resolve([activity(1), activity(3)])
    await flushPromises()
    expect(Object.keys(state.byAccount.value)).toEqual(['1', '3'])
    await vi.advanceTimersByTimeAsync(5000)
    expect(fetchActivity).toHaveBeenCalledTimes(2)
  })

  it('ignores a late response after switching pages and aborts its request', async () => {
    const old = deferred<AccountActivity[]>()
    fetchActivity.mockReturnValueOnce(old.promise).mockResolvedValueOnce([activity(2)])
    const { accounts, state } = setup()
    const oldSignal = fetchActivity.mock.calls[0]![1]!
    accounts.value = [{ id: 2, platform: 'openai' }]
    await nextTick()
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    old.resolve([activity(1)])
    await flushPromises()
    expect(Object.keys(state.byAccount.value)).toEqual(['2'])
  })

  it('stops while hidden, resumes immediately, and aborts on unmount', async () => {
    fetchActivity.mockResolvedValueOnce([activity(1)])
    const { state } = setup()
    await flushPromises()
    hidden = true
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(20000)
    expect(fetchActivity).toHaveBeenCalledTimes(1)
    const pending = deferred<AccountActivity[]>()
    fetchActivity.mockReturnValueOnce(pending.promise)
    hidden = false
    document.dispatchEvent(new Event('visibilitychange'))
    expect(fetchActivity).toHaveBeenCalledTimes(2)
    const signal = fetchActivity.mock.calls[1]![1]!
    wrapper!.unmount()
    wrapper = undefined
    expect(signal.aborted).toBe(true)
    pending.resolve([activity(9)])
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10000)
    expect(fetchActivity).toHaveBeenCalledTimes(2)
    expect(state.byAccount.value[1]).toBeDefined()
  })

  it('retains the last result on failure and protects a saved result from an older poll', async () => {
    fetchActivity.mockResolvedValueOnce([activity(1)]).mockRejectedValueOnce(new Error('offline'))
    const { state } = setup()
    await flushPromises()
    await state.refresh()
    expect(state.stale.value).toBe(true)
    expect(state.byAccount.value[1]).toEqual(activity(1))
    const old = deferred<AccountActivity[]>()
    const fresh = deferred<AccountActivity[]>()
    fetchActivity.mockReturnValueOnce(old.promise).mockReturnValueOnce(fresh.promise)
    void state.refresh()
    state.setQuality({ ...activity(1).quality, state: 'running' })
    old.resolve([activity(1)])
    await flushPromises()
    expect(state.byAccount.value[1]!.quality.state).toBe('running')
    fresh.resolve([{ ...activity(1), quality: { ...activity(1).quality, state: 'passed' } }])
    await flushPromises()
    expect(state.byAccount.value[1]!.quality.state).toBe('passed')
    expect(state.stale.value).toBe(false)
  })

  it('splits large pages into API-sized batches and does nothing for other platforms', async () => {
    setup(Array.from({ length: 201 }, (_, index) => ({ id: index + 1, platform: 'openai' })))
    await flushPromises()
    expect(fetchActivity.mock.calls.map(call => call[0].length)).toEqual([200, 1])
    wrapper!.unmount()
    fetchActivity.mockClear()
    setup([{ id: 2, platform: 'anthropic' }])
    await vi.advanceTimersByTimeAsync(10000)
    expect(fetchActivity).not.toHaveBeenCalled()
  })
})
