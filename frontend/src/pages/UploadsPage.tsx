import { useCallback, useEffect, useState } from 'react'
import { CloudUpload, RefreshCw, Trash2, XCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, formatDate } from '@/lib/api'

type QueueItem = {
  id: string
  fileName: string
  mimeType: string
  sizeBytes: string
  status: string
  error: string
  createdAt: string
  completedAt: string
  accountEmail: string
  folder: string
  resumable: boolean
}

const STATUS_TONE: Record<string, string> = {
  uploading: 'bg-blue-100 text-blue-700',
  pending: 'bg-amber-100 text-amber-700',
  in_progress: 'bg-blue-100 text-blue-700',
  completed: 'bg-emerald-100 text-emerald-700',
  failed: 'bg-red-100 text-red-700',
  cancelled: 'bg-slate-200 text-slate-600',
}

const ACTIVE_STATUSES = ['uploading', 'pending', 'in_progress']

export function UploadsPage() {
  const [items, setItems] = useState<QueueItem[]>([])
  const [counts, setCounts] = useState<Record<string, number>>({})
  const [filter, setFilter] = useState('all')
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [message, setMessage] = useState('')
  const [autoRefresh, setAutoRefresh] = useState(true)

  const load = useCallback(async () => {
    try {
      const data = await apiFetch<{ items: QueueItem[]; counts: Record<string, number> }>(`/uploads/queue?status=${filter}`)
      setItems(data.items)
      setCounts(data.counts)
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load uploads')
    } finally {
      setLoading(false)
    }
  }, [filter])

  useEffect(() => {
    load().catch(() => undefined)
  }, [load])

  // Poll while something is actually running.
  useEffect(() => {
    if (!autoRefresh) return
    const active = ACTIVE_STATUSES.reduce((sum, s) => sum + (counts[s] ?? 0), 0)
    const interval = window.setInterval(() => {
      load().catch(() => undefined)
    }, active > 0 ? 3000 : 15000)
    return () => window.clearInterval(interval)
  }, [autoRefresh, counts, load])

  async function cancel(item: QueueItem) {
    if (!confirm(`Cancel upload of "${item.fileName}"?`)) return
    setBusyId(item.id)
    try {
      await apiFetch(`/uploads/queue/${item.id}/cancel`, { method: 'POST' })
      await load()
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Cancel failed')
    } finally {
      setBusyId(null)
    }
  }

  async function remove(item: QueueItem) {
    setBusyId(item.id)
    try {
      await apiFetch(`/uploads/queue/${item.id}`, { method: 'DELETE' })
      await load()
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Remove failed')
    } finally {
      setBusyId(null)
    }
  }

  const activeCount = ACTIVE_STATUSES.reduce((sum, s) => sum + (counts[s] ?? 0), 0)

  return (
    <>
      <PageHeader
        title="Uploads"
        description="Resumable upload sessions: what finished, what is running, what failed."
        actions={
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={() => setAutoRefresh((v) => !v)}>
              <CloudUpload className="h-4 w-4" />Auto {autoRefresh ? 'On' : 'Off'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
              <RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh
            </Button>
          </div>
        }
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <div className="mt-5 grid gap-3 sm:grid-cols-4">
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Active</p><p className="mt-1 text-2xl font-extrabold text-blue-600">{activeCount}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Completed</p><p className="mt-1 text-2xl font-extrabold text-emerald-600">{counts.completed ?? 0}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Failed</p><p className="mt-1 text-2xl font-extrabold text-red-600">{counts.failed ?? 0}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Cancelled</p><p className="mt-1 text-2xl font-extrabold text-slate-500">{counts.cancelled ?? 0}</p></Card>
      </div>

      <div className="mt-4 flex flex-wrap gap-2">
        {['all', ...ACTIVE_STATUSES, 'completed', 'failed', 'cancelled'].map((s) => (
          <Button key={s} size="sm" variant={filter === s ? 'default' : 'outline'} onClick={() => setFilter(s)}>{s.replace('_', ' ')}</Button>
        ))}
      </div>

      <Card className="mt-4 p-4">
        {loading && items.length === 0 ? (
          <p className="text-sm text-slate-500">Loading...</p>
        ) : items.length === 0 ? (
          <div className="py-6 text-center">
            <CloudUpload className="mx-auto h-8 w-8 text-slate-300" />
            <p className="mt-2 text-sm font-semibold">Nothing in the queue.</p>
            <p className="mt-1 text-[12px] text-slate-500">Uploads appear here while running and after they finish.</p>
          </div>
        ) : (
          <ul className="grid gap-2">
            {items.map((item) => (
              <li key={item.id} className="flex flex-col gap-2 rounded-xl border border-slate-100 p-3 sm:flex-row sm:items-center sm:justify-between">
                <div className="min-w-0">
                  <p className="flex items-center gap-2 text-[13px] font-semibold">
                    <span className="truncate" title={item.fileName}>{item.fileName}</span>
                    <span className={`shrink-0 rounded-full px-2 py-0.5 text-[10px] font-bold uppercase ${STATUS_TONE[item.status] ?? 'bg-slate-100 text-slate-600'}`}>{item.status.replace('_', ' ')}</span>
                    {item.resumable ? <span className="shrink-0 rounded-full bg-slate-100 px-2 py-0.5 text-[10px] font-bold text-slate-600">resumable</span> : null}
                  </p>
                  <p className="mt-0.5 text-[11px] text-slate-500">
                    {formatBytes(item.sizeBytes)}
                    {item.accountEmail ? <> · {item.accountEmail}</> : <> · no account</>}
                    {item.folder ? <> · {item.folder}</> : null}
                    {item.createdAt ? <> · started {formatDate(item.createdAt)}</> : null}
                  </p>
                  {item.error ? <p className="mt-0.5 text-[11px] text-red-600">{item.error}</p> : null}
                </div>
                <div className="flex shrink-0 gap-2">
                  {ACTIVE_STATUSES.includes(item.status) ? (
                    <Button size="sm" variant="outline" onClick={() => cancel(item)} disabled={busyId === item.id}>
                      <XCircle className="h-4 w-4" />Cancel
                    </Button>
                  ) : (
                    <Button size="sm" variant="outline" onClick={() => remove(item)} disabled={busyId === item.id}>
                      <Trash2 className="h-4 w-4" />Remove
                    </Button>
                  )}
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </>
  )
}
