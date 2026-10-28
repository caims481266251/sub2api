import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account, CreateAccountRequest, CodexSessionImportRequest } from '@/types'
import { withDockerSystemProfile } from '@/utils/dockerSystemProfile'
const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))
import { getDockerSystemPool } from '@/api/admin/accounts'
beforeEach(() => { get.mockReset() })
describe('Docker system pool API and request types', () => {
  it('reads the admin pool without probe parameters', async () => {
    const data = { profiles: [], legacy_bound_account_count: 0, count_scope: 'all', checked_at: 'now' }
    get.mockResolvedValue({ data })
    expect(await getDockerSystemPool()).toBe(data)
    expect(get).toHaveBeenCalledWith('/admin/accounts/docker-system-pool')
  })
  it('propagates pool errors without manufacturing an empty snapshot', async () => {
    get.mockRejectedValue(new Error('offline'))
    await expect(getDockerSystemPool()).rejects.toThrow('offline')
  })
  it('sends auto/manual for new Docker requests and omits the field for normal requests', () => {
    const create: CreateAccountRequest = { name: 'test', platform: 'openai', type: 'oauth', credentials: {}, execution_mode: 'docker_kernel' }
    const session: CodexSessionImportRequest = { execution_mode: 'docker_kernel', update_existing: true }
    expect(withDockerSystemProfile(create, 'auto').docker_system_profile).toBe('auto')
    expect(withDockerSystemProfile(session, 'debian-12').docker_system_profile).toBe('debian-12')
    expect(withDockerSystemProfile({ ...create, execution_mode: 'normal', docker_system_profile: 'debian-12' }, 'auto')).not.toHaveProperty('docker_system_profile')
    const legacy: Pick<Account, 'docker_system_profile'> = { docker_system_profile: null }
    expect(legacy.docker_system_profile).toBeNull()
    expect(create).not.toHaveProperty('docker_system_profile')
  })
})
