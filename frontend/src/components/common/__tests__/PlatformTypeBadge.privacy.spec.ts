import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'
import PlatformTypeBadge from '../PlatformTypeBadge.vue'

function render(locale: 'zh' | 'en', privacyMode: string) {
  return mount(PlatformTypeBadge, {
    props: { platform: 'openai', type: 'oauth', planType: 'self_serve_business_prolite', privacyMode },
    global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] },
  })
}

describe('OpenAI privacy result badge', () => {
  it.each([
    ['zh', 'training_set_failed', '隐私未设置', '不代表帐号授权失败', 'bg-red-100'],
    ['zh', 'training_set_cf_blocked', '隐私受阻', '不代表帐号授权失败', 'bg-yellow-100'],
    ['en', 'training_set_failed', 'Privacy unset', 'not an account authorization failure', 'bg-red-100'],
    ['en', 'training_set_cf_blocked', 'Privacy blocked', 'not an account authorization failure', 'bg-yellow-100'],
  ] as const)('clarifies %s %s without changing its result color', (locale, mode, label, explanation, color) => {
    const wrapper = render(locale, mode)
    const badge = wrapper.get('[data-testid="privacy-badge"]')
    expect(badge.text()).toBe(label)
    expect(badge.attributes('title')).toContain(explanation)
    expect(badge.classes()).toContain(color)
    expect(wrapper.text()).toContain('Business Premium')
    expect(wrapper.text()).toContain('OAuth')
    wrapper.unmount()
  })

  it('updates only when the actual privacy result changes', async () => {
    const wrapper = render('zh', 'training_set_failed')
    await wrapper.setProps({ planType: 'plus' })
    expect(wrapper.get('[data-testid="privacy-badge"]').text()).toBe('隐私未设置')
    await wrapper.setProps({ privacyMode: 'training_off' })
    expect(wrapper.get('[data-testid="privacy-badge"]').text()).toBe('Private')
    expect(wrapper.get('[data-testid="privacy-badge"]').classes()).toContain('bg-green-100')
    await wrapper.setProps({ privacyMode: 'training_set_cf_blocked' })
    expect(wrapper.get('[data-testid="privacy-badge"]').text()).toBe('隐私受阻')
    wrapper.unmount()
  })

  it.each(['', 'unknown'])('does not claim a result for %s', mode => {
    const wrapper = render('zh', mode)
    expect(wrapper.find('[data-testid="privacy-badge"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('does not show an OAuth privacy result on API key accounts', async () => {
    const wrapper = render('zh', 'training_set_failed')
    await wrapper.setProps({ type: 'apikey' })
    expect(wrapper.find('[data-testid="privacy-badge"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
