export interface HostingErrorRuleDraft { error_code: number; keywords: string; duration_minutes: number; description: string }
export function hostingErrorRuleDrafts(value: unknown): HostingErrorRuleDraft[] {
  if (!Array.isArray(value)) return []
  return value.map(rule => ({ error_code: Number(rule.error_code), keywords: Array.isArray(rule.keywords) ? rule.keywords.join(', ') : '', duration_minutes: Number(rule.duration_minutes), description: String(rule.description ?? '') }))
}
export function buildHostingErrorRules(rules: HostingErrorRuleDraft[]) {
  if (!rules.length || rules.length > 20) return null
  const normalized = rules.map(rule => ({ ...rule, description: rule.description.trim(), keywords: rule.keywords.split(/[,，\n]/).map(keyword => keyword.trim()).filter(Boolean) }))
  if (normalized.some(rule => !Number.isInteger(rule.error_code) || rule.error_code < 400 || rule.error_code > 599 || !Number.isInteger(rule.duration_minutes) || rule.duration_minutes < 1 || rule.duration_minutes > 10080 || !rule.keywords.length || rule.keywords.length > 20 || rule.keywords.some(word => new TextEncoder().encode(word).length > 256) || [...rule.description].length > 256)) return null
  return normalized
}
