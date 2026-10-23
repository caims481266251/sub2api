import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import VMKernelStatusBadge from '../VMKernelStatusBadge.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.endsWith('vmRuntimeReady') ? '正常' : '异常' }) }))

describe('VM account status', () => {
  it.each(['tcg', 'kvm'] as const)('shows only normal for a ready %s runtime', (actual_accelerator) => {
    const wrapper = mount(VMKernelStatusBadge, { props: { status: { phase: 'kernel_ready', ready: true, capacity: 30, actual_accelerator } } })
    expect(wrapper.text()).toBe('VM · 正常')
    expect(wrapper.classes()).toContain('bg-green-100')
    expect(wrapper.attributes('title')).toBeUndefined()
  })
  it.each(['preparing', 'stopped', 'stale', 'error'])('shows only abnormal when unavailable (%s)', (phase) => {
    const wrapper = mount(VMKernelStatusBadge, { props: { status: { phase, ready: false, capacity: 30, error_code: 'kernel_attempt_unknown' } } })
    expect(wrapper.text()).toBe('VM · 异常')
    expect(wrapper.classes()).toContain('bg-red-100')
    expect(wrapper.attributes('title')).toBeUndefined()
  })
  it('does not claim normal before runtime status is available', () => {
    expect(mount(VMKernelStatusBadge).text()).toBe('VM · 异常')
  })
})
