import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import HostingTwoFALoginForm from '../HostingTwoFALoginForm.vue'

const api = vi.hoisted(() => ({ startTwoFALogin: vi.fn(), getTwoFALogin: vi.fn(), completeTwoFALogin: vi.fn(), deleteTwoFALogin: vi.fn() }))
vi.mock('@/api/hosting', async original => ({ ...await original<typeof import('@/api/hosting')>(), hostingAPI: api }))
vi.mock('vue-i18n', async original => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))
let wrapper: ReturnType<typeof mount>
const settings = { hosting_pool_id: 31, name: 'Configured', concurrency: 8, priority: 20, credentials: {}, extra: {} }
async function renderAndLogin() {
  wrapper = mount(HostingTwoFALoginForm, { props: { settings, replaceId: 17, host: 'login.example.test' } })
  await wrapper.get('#hosting-login-email').setValue('user@example.com')
  await wrapper.get('#hosting-login-password').setValue('password-canary')
  await wrapper.get('#hosting-login-mfa').setValue('mfa-canary')
  await wrapper.get('button.btn-primary').trigger('click'); await flushPromises()
}
beforeEach(() => {
  vi.useFakeTimers()
  Object.values(api).forEach(mock => mock.mockReset())
  api.startTwoFALogin.mockResolvedValue({ id: 'scoped-job', status: 'succeeded' })
  api.completeTwoFALogin.mockResolvedValue({ id: 17 })
  api.deleteTwoFALogin.mockResolvedValue({})
})
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })
describe('Hosted account 2FA login', () => {
  it('uses user endpoints and saves selected configuration without exposing tokens or storing passwords', async () => {
    await renderAndLogin()
    expect(api.startTwoFALogin).toHaveBeenCalledWith({ email: 'user@example.com', password: 'password-canary', mfa_secret: 'mfa-canary' }, 17, 31)
    expect(api.completeTwoFALogin).toHaveBeenCalledWith('scoped-job', settings)
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.get<HTMLInputElement>('#hosting-login-password').element.value).toBe('')
    expect(wrapper.get<HTMLInputElement>('#hosting-login-mfa').element.value).toBe('')
  })
  it('retries saving a verified login without running a second login', async () => {
    api.completeTwoFALogin.mockRejectedValueOnce({ message: 'Seats full' })
    await renderAndLogin()
    expect(wrapper.get('[role="alert"]').text()).toBe('Seats full')
    expect(wrapper.find('#hosting-login-password').exists()).toBe(false)
    await wrapper.get('button.btn-primary').trigger('click'); await flushPromises()
    expect(api.startTwoFALogin).toHaveBeenCalledTimes(1)
    expect(api.completeTwoFALogin).toHaveBeenCalledTimes(2)
    expect(wrapper.emitted('saved')).toHaveLength(1)
  })
  it('cancels a running login and prevents subsequent polling or creation', async () => {
    api.startTwoFALogin.mockResolvedValue({ id: 'scoped-job', status: 'running' })
    await renderAndLogin()
    await wrapper.get('button.btn-secondary').trigger('click'); await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    expect(api.deleteTwoFALogin).toHaveBeenCalledWith('scoped-job')
    expect(api.getTwoFALogin).not.toHaveBeenCalled()
    expect(api.completeTwoFALogin).not.toHaveBeenCalled()
    expect(wrapper.get<HTMLInputElement>('#hosting-login-password').element.value).toBe('')
  })
  it('cleans up a job created after the form is unmounted', async () => {
    let resolve!: (value: { id: string; status: string }) => void
    api.startTwoFALogin.mockReturnValue(new Promise(done => { resolve = done }))
    await renderAndLogin(); wrapper.unmount()
    resolve({ id: 'late-job', status: 'running' }); await flushPromises()
    expect(api.deleteTwoFALogin).toHaveBeenCalledWith('late-job')
    expect(api.completeTwoFALogin).not.toHaveBeenCalled()
  })
})
