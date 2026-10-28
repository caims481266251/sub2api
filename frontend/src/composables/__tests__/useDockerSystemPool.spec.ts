import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, nextTick, ref } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { useDockerSystemPool } from '../useDockerSystemPool'
import type { DockerSystemPool } from '@/types'

const { fetchPool } = vi.hoisted(() => ({ fetchPool: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getDockerSystemPool: fetchPool } } }))
const pool: DockerSystemPool = {
  profiles: [{ profile_id: 'debian-12', display_name: 'Debian 12', available: true, bound_account_count: 8 }],
  legacy_bound_account_count: 3, count_scope: 'all', checked_at: '2026-10-27T10:00:00Z',
}
let wrapper: VueWrapper | undefined
function setup(visible = true, docker = true) {
  const open = ref(visible), enabled = ref(docker)
  let state!: ReturnType<typeof useDockerSystemPool>
  wrapper = mount(defineComponent({ setup() { state = useDockerSystemPool(open, enabled); return () => null } }))
  return { open, enabled, state }
}
beforeEach(() => { fetchPool.mockReset().mockResolvedValue(pool) })
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })

describe('useDockerSystemPool', () => {
  it('loads on open, Docker entry and explicit refresh, with no polling', async () => {
    vi.useFakeTimers()
    const { enabled, state } = setup(true, false)
    expect(fetchPool).not.toHaveBeenCalled()
    enabled.value = true
    await nextTick(); await flushPromises()
    expect(state.canSubmit.value).toBe(true)
    expect(fetchPool).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(60000)
    expect(fetchPool).toHaveBeenCalledTimes(1)
    await state.refresh()
    expect(fetchPool).toHaveBeenCalledTimes(2)
  })
  it('retains prior counts on failure, blocks submission and recovers on refresh', async () => {
    const { state } = setup()
    await flushPromises()
    fetchPool.mockRejectedValueOnce(new Error('offline'))
    await state.refresh()
    expect(state.error.value).toBe(true)
    expect(state.pool.value?.profiles[0].bound_account_count).toBe(8)
    expect(state.canSubmit.value).toBe(false)
    await state.refresh()
    expect(state.error.value).toBe(false)
    expect(state.canSubmit.value).toBe(true)
  })
  it('rejects unavailable selections and keeps absent profile counts unknown', async () => {
    const { state } = setup()
    await flushPromises()
    state.selection.value = 'fedora-44'
    expect(state.canSubmit.value).toBe(false)
    expect(state.profiles.value.find(profile => profile.profile_id === 'fedora-44')?.bound_account_count).toBeNull()
  })
  it.each([true, false])('rejects duplicate profile IDs even when the first availability is %s', async firstAvailable => {
    const { state } = setup()
    await flushPromises()
    state.selection.value = 'debian-12'
    fetchPool.mockResolvedValueOnce({ ...pool, profiles: [
      { ...pool.profiles[0], available: firstAvailable },
      { ...pool.profiles[0], available: !firstAvailable },
    ] })
    await state.refresh()
    expect(state.error.value).toBe(true)
    expect(state.canSubmit.value).toBe(false)
    state.selection.value = 'auto'
    expect(state.canSubmit.value).toBe(false)
  })
  it('blocks a selected profile missing from the refreshed snapshot', async () => {
    const { state } = setup()
    await flushPromises()
    state.selection.value = 'debian-12'
    expect(state.canSubmit.value).toBe(true)
    fetchPool.mockResolvedValueOnce({ ...pool, profiles: [{
      profile_id: 'fedora-44', display_name: 'Fedora 44', available: true, bound_account_count: 2,
    }] })
    await state.refresh()
    expect(state.error.value).toBe(false)
    expect(state.canSubmit.value).toBe(false)
    const missing = state.profiles.value.find(profile => profile.profile_id === 'debian-12')
    expect(missing?.available).toBe(false)
    expect(missing?.bound_account_count).toBeNull()
    state.selection.value = 'auto'
    expect(state.canSubmit.value).toBe(true)
  })
  it('resets to auto on reopen and ignores late responses after close', async () => {
    let resolve!: (value: DockerSystemPool) => void
    const { state, open } = setup()
    await flushPromises()
    state.selection.value = 'debian-12'
    fetchPool.mockReturnValueOnce(new Promise(done => { resolve = done }))
    const pending = state.refresh()
    open.value = false; await nextTick()
    resolve(pool); await pending
    expect(state.pool.value).toBeNull()
    open.value = true; await nextTick(); await flushPromises()
    expect(state.selection.value).toBe('auto')
    expect(state.canSubmit.value).toBe(true)
  })
  it('treats invalid counts as an error rather than displaying zero', async () => {
    fetchPool.mockResolvedValue({ ...pool, legacy_bound_account_count: undefined })
    const { state } = setup()
    await flushPromises()
    expect(state.error.value).toBe(true)
    expect(state.canSubmit.value).toBe(false)
    expect(state.pool.value).toBeNull()
  })
})
