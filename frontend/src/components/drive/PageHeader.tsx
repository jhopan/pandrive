import type { ReactNode } from 'react'
import { useI18n } from '@/lib/i18n'

// Titles/descriptions are translated centrally so every page gets ID/EN for free; passing
// translated strings from callers stays possible because ReactNode always wins.
export function PageHeader({ title, description, actions }: { title: ReactNode; description?: string; actions?: ReactNode }) {
  const { t, lang } = useI18n()
  const shown = lang === 'id' && typeof title === 'string' ? t(title) : title
  const desc = lang === 'id' && description ? t(description) : description
  return (
    <div className="mt-2.5 flex flex-col gap-2 sm:mt-3.5 sm:flex-row sm:items-center sm:justify-between">
      <div className="min-w-0">
        <h1 className="text-lg font-extrabold tracking-tight sm:text-[22px] lg:text-[28px]">{shown}</h1>
        {desc ? <p className="mt-1 text-sm text-slate-500">{desc}</p> : null}
      </div>
      {actions ? (
        <div className="flex flex-wrap gap-2 sm:shrink-0 sm:flex-nowrap sm:justify-end">
          {actions}
        </div>
      ) : null}
    </div>
  )
}
