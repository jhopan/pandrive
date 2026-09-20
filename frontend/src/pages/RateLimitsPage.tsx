import { useCallback, useEffect, useState } from 'react'
import { Activity, Gauge, Info, RefreshCw, Timer } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch } from '@/lib/api'

type ConfigState = {
  id: string
  label: string
  status: string
  flowStarts: number
  windowStart: string
  windowResetsInSeconds: number
  lastUsedAt: string
  accounts: number
}
type RateLimits = {
  windowSeconds: number
  limit: number
  threshold: number
  requestsLast100s: number
  peakLast100s: number
  history: number[]
  configs: ConfigState[]
  notes: string[]
}

export function RateLimitsPage() {
  const [data, setData] = useState<RateLimits | null>(null)
  const [loading, setLoading] = useState(true)
  const [autoRefresh, setAutoRefresh] = useState(true)
  const [message, setMessage] = useState('')
  const [tick, setTick] = useState(0)

  const load = useCallback(async () => {
    try {
      setData(await apiFetch<RateLimits>('/system/rate-limits'))
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load rate limits')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load().catch(() => undefined)
  }, [load])

  useEffect(() => {
    if (!autoRefresh) return
    const interval = window.setInterval(() => {
      load().catch(() => undefined)
      setTick((t) => t + 1)
    }, 5000)
    return () => window.clearInterval(interval)
  }, [autoRefresh, load])

  const total = data?.requestsLast100s ?? 0
  const limit = data?.limit ?? 10000
  const threshold = data?.threshold ?? 8000
  const pct = Math.min(100, (total / limit) * 100)
  const tone = total >= limit ? 'bg-red-500' : total >= threshold ? 'bg-amber-500' : 'bg-emerald-500'
  const statusLabel = total >= limit ? 'At limit' : total >= threshold ? 'Near limit - config switching active' : 'Healthy'
  const maxBar = Math.max(1, ...(data?.history ?? [1]))

  return (
    <>
      <PageHeader
        title="Rate Limits"
        description={`Google allows ${limit.toLocaleString()} requests per ${data?.windowSeconds ?? 100}s per OAuth client project.`}
        actions={
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={() => setAutoRefresh((v) => !v)}>
              <Timer className="h-4 w-4" />Auto {autoRefresh ? 'On' : 'Off'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
              <RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh
            </Button>
          </div>
        }
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <div className="mt-5 grid gap-4 sm:grid-cols-3">
        <Card className="p-4">
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><Gauge className="h-4 w-4" />Requests (last {data?.windowSeconds ?? 100}s)</p>
          <p className="mt-1 text-2xl font-extrabold">{total.toLocaleString()}</p>
          <div className="mt-2 h-2 overflow-hidden rounded-full bg-slate-100">
            <div className={`h-full rounded-full transition-all ${tone}`} style={{ width: `${pct}%` }} />
          </div>
          <p className="mt-1.5 text-[11px] text-slate-500">{statusLabel} · {pct.toFixed(1)}% of window</p>
        </Card>
        <Card className="p-4">
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><Activity className="h-4 w-4" />Peak this session</p>
          <p className="mt-1 text-2xl font-extrabold">{(data?.peakLast100s ?? 0).toLocaleString()}</p>
          <p className="mt-1.5 text-[11px] text-slate-500">Highest 100s total since the process started</p>
        </Card>
        <Card className="p-4">
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><Timer className="h-4 w-4" />Switch threshold</p>
          <p className="mt-1 text-2xl font-extrabold">{threshold.toLocaleString()}</p>
          <p className="mt-1.5 text-[11px] text-slate-500">Configs at or above this stop being selected</p>
        </Card>
      </div>

      <Card className="mt-4 p-4">
        <div className="flex items-center justify-between">
          <h2 className="text-[16px] font-bold">Last {data?.history.length ?? 100} seconds</h2>
          <span className="text-[11px] text-slate-500">peak {maxBar} req/s</span>
        </div>
        <div className="mt-3 flex h-24 items-end gap-[2px]">
          {(data?.history ?? []).map((value, index) => (
            <div
              key={index}
              className={`flex-1 rounded-sm ${value >= maxBar * 0.8 ? 'bg-amber-500' : 'bg-blue-500'}`}
              style={{ height: `${Math.max(value > 0 ? 6 : 2, (value / maxBar) * 100)}%`, opacity: value > 0 ? 1 : 0.25 }}
              title={`${value} request${value === 1 ? '' : 's'}`}
            />
          ))}
        </div>
        <p className="mt-2 text-[11px] text-slate-400">Oldest on the left, now on the right. Refresh tick {tick}.</p>
      </Card>

      <Card className="mt-4 p-4">
        <h2 className="text-[16px] font-bold">OAuth configs</h2>
        <p className="mt-1 text-[12px] text-slate-500">Uploads and syncs rotate across these to stay under the per-project limit.</p>
        <div className="mt-3 overflow-x-auto">
          <table className="w-full text-left text-[13px]">
            <thead>
              <tr className="border-b border-slate-100 text-[11px] uppercase text-slate-400">
                <th className="py-2 pr-3 font-bold">Label</th>
                <th className="py-2 pr-3 font-bold">Status</th>
                <th className="py-2 pr-3 font-bold">OAuth flow starts</th>
                <th className="py-2 pr-3 font-bold">Window resets</th>
                <th className="py-2 pr-3 font-bold">Accounts</th>
                <th className="py-2 font-bold">Last used</th>
              </tr>
            </thead>
            <tbody>
              {(data?.configs ?? []).map((config) => (
                <tr key={config.id} className="border-b border-slate-50">
                  <td className="py-2 pr-3 font-medium">{config.label || config.id.slice(0, 12)}</td>
                  <td className="py-2 pr-3"><span className={config.status === 'active' ? 'text-emerald-600' : 'text-slate-400'}>{config.status}</span></td>
                  <td className="py-2 pr-3 text-slate-600">{config.flowStarts}</td>
                  <td className="py-2 pr-3 text-slate-600">{config.windowResetsInSeconds > 0 ? `${config.windowResetsInSeconds}s` : 'idle'}</td>
                  <td className="py-2 pr-3 text-slate-600">{config.accounts}</td>
                  <td className="py-2 text-slate-500">{config.lastUsedAt ? config.lastUsedAt.replace('T', ' ').slice(0, 19) : 'never'}</td>
                </tr>
              ))}
              {data && data.configs.length === 0 ? (
                <tr><td colSpan={6} className="py-3 text-slate-500">No OAuth configs saved yet.</td></tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </Card>

      {data && data.notes.length > 0 ? (
        <Card className="mt-4 p-4">
          <h2 className="flex items-center gap-2 text-[15px] font-bold"><Info className="h-4 w-4 text-blue-500" />How these numbers are counted</h2>
          <ul className="mt-2 grid gap-1 text-[12px] text-slate-600">
            {data.notes.map((note) => <li key={note}>- {note}</li>)}
          </ul>
        </Card>
      ) : null}
    </>
  )
}
