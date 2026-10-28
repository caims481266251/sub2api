import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import DockerSystemSelector from '../DockerSystemSelector.vue'
import DockerSystemProfileLabel from '../DockerSystemProfileLabel.vue'
import { DOCKER_SYSTEM_PROFILES } from '@/utils/dockerSystemProfile'
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string, args?: unknown) => key + (args ? JSON.stringify(args) : '') }) }))
const profiles = DOCKER_SYSTEM_PROFILES.map((profile, index) => ({ ...profile, available: index !== 4, bound_account_count: index + 1,
  unavailable_reason: index === 4 ? 'profile_image_missing' : '' }))
const pool = { profiles, legacy_bound_account_count: 9, count_scope: 'visible' as const, checked_at: '2026-10-27T10:00:00Z' }
const props = { modelValue: 'auto' as const, profiles, pool, loading: false, error: false }

describe('Docker system selector and readonly binding', () => {
  it('shows auto plus five systems, counts, scope and unavailable reasons', async () => {
    const wrapper = mount(DockerSystemSelector, { props })
    expect(wrapper.findAll('input[type="radio"]')).toHaveLength(6)
    expect(wrapper.text()).toContain('"count":9')
    expect(wrapper.text()).toContain('admin.accounts.dockerSystem.visibleCounts')
    expect(wrapper.text()).toContain('admin.accounts.dockerSystem.reasonImageMissing')
    expect(wrapper.text()).not.toContain('profile_image_missing')
    expect(wrapper.get('[data-testid="docker-system-fedora-44"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="docker-system-debian-12"]').setValue(true)
    expect(wrapper.emitted('update:modelValue')).toEqual([['debian-12']])
    await wrapper.get('[data-testid="docker-system-refresh"]').trigger('click')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
  })
  it('shows errors instead of stale counts and disables all choices', () => {
    const wrapper = mount(DockerSystemSelector, { props: { ...props, error: true } })
    expect(wrapper.get('[role="alert"]').text()).toContain('loadFailed')
    expect(wrapper.text()).not.toContain('boundCount')
    expect(wrapper.findAll('input').every(input => input.attributes('disabled') !== undefined)).toBe(true)
  })
  it.each([
    ['architecture_not_supported', 'reasonArchitecture'],
    ['profile_not_installed', 'reasonNotInstalled'],
    ['template_not_installed', 'reasonNotInstalled'],
    ['profile_image_missing', 'reasonImageMissing'],
    ['profile_validation_failed', 'reasonValidationFailed'],
    ['runtime_catalog_unverified', 'reasonCatalogUnverified'],
    ['runtime_host_unavailable', 'reasonHostUnavailable'],
    ['internal failure at /private/runtime/credential.json', 'unavailable'],
  ])('renders a safe translated reason for %s', (reason, key) => {
    const wrapper = mount(DockerSystemSelector, { props: { ...props,
      profiles: profiles.map(profile => ({ ...profile, available: false, unavailable_reason: reason })),
    } })
    expect(wrapper.text()).toContain('admin.accounts.dockerSystem.' + key)
    expect(wrapper.text()).not.toContain(reason)
    expect(wrapper.findAll('input').every(input => input.attributes('disabled') !== undefined)).toBe(true)
  })
  it.each([['debian-12', 'Debian 12'], [null, 'admin.accounts.dockerSystem.legacy'], ['future-profile', 'future-profile']])('renders actual binding %s without an editor', (profile, text) => {
    const wrapper = mount(DockerSystemProfileLabel, { props: { profile } })
    expect(wrapper.text()).toContain(text!)
    expect(wrapper.find('input,select,button').exists()).toBe(false)
  })
})
