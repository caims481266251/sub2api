export const HOSTING_IMPORT_MAX_BYTES = 1024 * 1024

export class HostingImportInputError extends Error {
  constructor(readonly translationKey: string) { super(translationKey) }
}

/** Split formatted JSON, an array, or one auth.json object per line into single-account requests. */
export function parseHostingCredentials(text: string): string[] {
  return parseHostingImport(text).map(account => account.content)
}

export function parseHostingImport(text: string): { content: string; name?: string }[] {
  if (new TextEncoder().encode(text).length > HOSTING_IMPORT_MAX_BYTES) {
    throw new HostingImportInputError('hosting.fileTooLarge')
  }
  const trimmed = text.trim()
  if (!trimmed) throw new HostingImportInputError('hosting.importEmpty')
  let parsed: unknown
  try {
    parsed = JSON.parse(trimmed)
  } catch {
    try { parsed = trimmed.split(/\r?\n/).filter(line => line.trim()).map(line => JSON.parse(line)) }
    catch { throw new HostingImportInputError('hosting.invalidFile') }
  }
  const bundle = parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed as Record<string, unknown> : null
  if (bundle && 'accounts' in bundle && (
    !Array.isArray(bundle.accounts) ||
    (bundle.type !== undefined && bundle.type !== '' && !['sub2api-data', 'sub2api-bundle'].includes(String(bundle.type))) ||
    (bundle.version !== undefined && bundle.version !== 0 && bundle.version !== 1)
  )) throw new HostingImportInputError('hosting.invalidFile')
  const accounts = bundle && Array.isArray(bundle.accounts) ? bundle.accounts : Array.isArray(parsed) ? parsed : [parsed]
  if (!accounts.length) throw new HostingImportInputError('hosting.importEmpty')
  return accounts.map(account => {
    if (!account || typeof account !== 'object' || Array.isArray(account)) {
      throw new HostingImportInputError('hosting.invalidFile')
    }
    const entry = account as Record<string, unknown>
    const exported = 'credentials' in entry
    if (exported && (entry.platform !== 'openai' || entry.type !== 'oauth')) {
      throw new HostingImportInputError('hosting.importUnsupportedAccount')
    }
    if (exported && (!entry.credentials || typeof entry.credentials !== 'object' || Array.isArray(entry.credentials))) {
      throw new HostingImportInputError('hosting.invalidFile')
    }
    const candidate = exported ? entry.credentials as Record<string, unknown> : entry
    const tokens = candidate.tokens
    const nested = tokens && typeof tokens === 'object' && !Array.isArray(tokens) ? (tokens as Record<string, unknown>).refresh_token : undefined
    const refreshToken = typeof nested === 'string' && nested.trim() ? nested : candidate.refresh_token
    if (typeof refreshToken !== 'string' || !refreshToken.trim()) {
      throw new HostingImportInputError('hosting.importMissingRefreshToken')
    }
    return {
      // The hosted endpoint revalidates identity from the refresh token. Never copy
      // public groups, ownership, proxies, rates or admin-only account settings.
      content: exported ? JSON.stringify({ refresh_token: refreshToken }) : JSON.stringify(account),
      ...(exported && typeof entry.name === 'string' && entry.name.trim() ? { name: entry.name.trim().slice(0, 100) } : {})
    }
  })
}

export async function readHostingImportFile(file: File): Promise<string> {
  if (file.size > HOSTING_IMPORT_MAX_BYTES) throw new HostingImportInputError('hosting.fileTooLarge')
  if (typeof file.text === 'function') return file.text()
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(new HostingImportInputError('hosting.importReadFailed'))
    reader.readAsText(file)
  })
}
