export const OPENAI_USAGE_CONCURRENCY_KEY = 'openai_usage_concurrency'
export interface OpenAIUsageConcurrency {
  enabled: boolean
  threshold_percent: number
  concurrency: number
}
export function readOpenAIUsageConcurrency(extra?: Record<string, unknown> | null): OpenAIUsageConcurrency {
  const raw = extra?.[OPENAI_USAGE_CONCURRENCY_KEY] as Partial<OpenAIUsageConcurrency> | undefined
  return {
    enabled: raw?.enabled === true,
    threshold_percent: typeof raw?.threshold_percent === 'number' ? raw.threshold_percent : 5,
    concurrency: typeof raw?.concurrency === 'number' ? raw.concurrency : 6
  }
}
