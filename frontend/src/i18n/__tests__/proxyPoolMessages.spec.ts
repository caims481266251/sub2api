import { describe, expect, it } from 'vitest'
import { baseCompile } from '@intlify/message-compiler'
import en from '../locales/en/proxyPool'
import zh from '../locales/zh/proxyPool'

function messages(value: unknown): string[] {
  return typeof value === 'string' ? [value] : Object.values(value as Record<string, unknown>).flatMap(messages)
}
describe('proxy pool translations', () => {
  it.each(['en', 'zh'] as const)('compiles %s messages and displays literal URL authentication syntax', locale => {
    const all = { en: { proxyPool: en }, zh: { proxyPool: zh } }
    const errors: string[] = []
    for (const message of messages(all[locale])) baseCompile(message, { onError: error => errors.push(error.message) })
    expect(errors).toEqual([])
    // Vitest uses the runtime-only build without production's JIT define.
    // Execute the compiler output for this repository-owned literal instead.
    const compiled = baseCompile(all[locale].proxyPool.placeholder)
    const render = new Function(`return ${compiled.code}`)() as (ctx: { normalize: (parts: string[]) => string }) => string
    expect(render({ normalize: parts => parts.join('') })).toContain('http://username:password@host:port')
  })
})
