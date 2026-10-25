import { describe, expect, it } from 'vitest'
import { HOSTING_IMPORT_MAX_BYTES, HostingImportInputError, parseHostingCredentials, parseHostingImport } from '../hostingImport'

describe('hosted Codex credential input', () => {
  it('accepts admin exports and preserves names without public ownership, proxy or billing settings', () => {
    const account = { name: 'My Codex', platform: 'openai', type: 'oauth', owner_user_id: 99, proxy_key: 'private', rate_multiplier: 9, credentials: { refresh_token: 'refresh', access_token: 'old-access' } }
    const expected = [{ name: 'My Codex', content: '{"refresh_token":"refresh"}' }]
    expect(parseHostingImport(JSON.stringify({ type: 'sub2api-data', version: 1, accounts: [account], proxies: [] }))).toEqual(expected)
    expect(parseHostingImport(JSON.stringify([account]))).toEqual(expected)
    expect(() => parseHostingImport(JSON.stringify({ type: 'sub2api-data', version: 2, accounts: [account] }))).toThrow('hosting.invalidFile')
    expect(() => parseHostingImport(JSON.stringify([{ ...account, platform: 'anthropic' }]))).toThrow('hosting.importUnsupportedAccount')
    expect(() => parseHostingImport(JSON.stringify([{ ...account, type: 'apikey' }]))).toThrow('hosting.importUnsupportedAccount')
  })
  it('accepts formatted JSON, arrays and one JSON object per line', () => {
    const first = { tokens: { refresh_token: 'first' } }, second = { refresh_token: 'second' }
    expect(parseHostingCredentials(JSON.stringify(first, null, 2))).toEqual([JSON.stringify(first)])
    const expected = [JSON.stringify(first), JSON.stringify(second)]
    expect(parseHostingCredentials(JSON.stringify([first, second]))).toEqual(expected)
    expect(parseHostingCredentials(`${JSON.stringify(first)}\r\n\r\n${JSON.stringify(second)}`)).toEqual(expected)
  })
  it('rejects a malformed or unsupported entry before any account is submitted', () => {
    for (const input of ['[]', 'null', '"token"', '{}', '{"tokens":{"refresh_token":123}}', '[{"refresh_token":"valid"},{}]', '{"refresh_token":"valid"}\nnot JSON']) {
      expect(() => parseHostingCredentials(input)).toThrow(HostingImportInputError)
    }
  })
  it('enforces the same byte limit as the backend', () => {
    expect(() => parseHostingCredentials(`{"refresh_token":"ok","padding":"${'中'.repeat(HOSTING_IMPORT_MAX_BYTES / 2)}"}`)).toThrow('hosting.fileTooLarge')
  })
})
