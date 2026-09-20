import { useCallback, useEffect, useMemo, useState } from 'react'
import { Clock, Download, FileText, FolderOpen, RefreshCw, Star } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { API_URL, apiFetch, formatBytes, formatDate } from '@/lib/api'
import { getAccessToken } from '@/lib/auth'

type RecentFile = {
  id: string
  name: string
  mimeType: string
  sizeBytes: string
  updatedAt: string
  createdAt: string
  accountEmail: string
  folder: { id: string; name: string } | null
  starred: boolean
}
type Account = { id: string; email: string }

// Day buckets keep a long list scannable without extra API calls.
function dayBucket(iso: string): string {
  if (!iso) return 'Earlier'
  const date = new Date(iso.replace(' ', 'T'))
  if (Number.isNaN(date.getTime())) return 'Earlier'
  const startOfToday = new Date()
  startOfToday.setHours(0, 0, 0, 0)
  const diffDays = Math.floor((startOfToday.getTime() - date.getTime()) / 86400000)
  if (diffDays < 0) return 'Today'
  if (diffDays === 0) return 'Today'
  if (diffDays === 1) return 'Yesterday'
  if (diffDays <= 7) return 'Earlier this week'
  if (diffDays <= 30) return 'This month'
  return 'Earlier'
}

export function RecentPage() {
  const [files, setFiles] = useState<RecentFile[]>([])
  const [accounts, setAccounts] = useState<Account[]>([])
  const [accountId, setAccountId] = useState('all')
  const [limit, setLimit] = useState(50)
  const [stats, setStats] = useState({ last24h: 0, last7d: 0 })
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [message, setMessage] = useState('')

  const load = useCallback(async (nextLimit = limit) => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ limit: String(nextLimit) })
      if (accountId !== 'all') params.set('accountId', accountId)
      const data = await apiFetch<{ files: RecentFile[]; last24h: number; last7d: number }>(`/recent?${params.toString()}`)
      setFiles(data.files)
      setStats({ last24h: data.last24h, last7d: data.last7d })
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load recent files')
    } finally {
      setLoading(false)
    }
  }, [accountId, limit])

  useEffect(() => {
    load().catch(() => undefined)
  }, [load])

  useEffect(() => {
    apiFetch<{ accounts: Account[] }>('/connected-accounts').then((d) => setAccounts(d.accounts)).catch(() => undefined)
  }, [])

  const grouped = useMemo(() => {
    const buckets = new Map<string, RecentFile[]>()
    for (const file of files) {
      const key = dayBucket(file.updatedAt)
      const list = buckets.get(key) ?? []
      list.push(file)
      buckets.set(key, list)
    }
    return [...buckets.entries()]
  }, [files])

  async function download(file: RecentFile) {
    try {
      const response = await fetch(`${API_URL}/files/${file.id}/download`, { headers: { Authorization: `Bearer ${getAccessToken()}` } })
      if (!response.ok) throw new Error('Download failed')
      const blob = await response.blob()
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = file.name
      anchor.click()
      URL.revokeObjectURL(url)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Download failed')
    }
  }

  async function toggleStar(file: RecentFile) {
    setBusyId(file.id)
    const next = !file.starred
    setFiles((prev) => prev.map((f) => (f.id === file.id ? { ...f, starred: next } : f)))
    try {
      await apiFetch(`/files/${file.id}/star`, { method: 'POST', body: JSON.stringify({ starred: next }) })
    } catch (error) {
      setFiles((prev) => prev.map((f) => (f.id === file.id ? { ...f, starred: !next } : f)))
      setMessage(error instanceof Error ? error.message : 'Failed to update starred')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <>
      <PageHeader
        title="Recent"
        description="Files by last change, newest first."
        actions={<Button variant="outline" size="sm" onClick={() => load()} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <div className="mt-5 grid gap-3 sm:grid-cols-3">
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Changed today</p><p className="mt-1 text-2xl font-extrabold text-blue-600">{stats.last24h}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">This week</p><p className="mt-1 text-2xl font-extrabold">{stats.last7d}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Shown</p><p className="mt-1 text-2xl font-extrabold">{files.length}</p></Card>
      </div>

      <div className="mt-4 flex flex-wrap items-end gap-3">
        <label className="grid gap-1.5 text-xs font-bold text-slate-500">
          Account
          <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={accountId} onChange={(e) => setAccountId(e.target.value)}>
            <option value="all">All accounts</option>
            {accounts.map((a) => <option key={a.id} value={a.id}>{a.email}</option>)}
          </select>
        </label>
        <label className="grid gap-1.5 text-xs font-bold text-slate-500">
          How many
          <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={String(limit)} onChange={(e) => { const next = Number(e.target.value); setLimit(next); load(next).catch(() => undefined) }}>
            {[25, 50, 100, 200].map((n) => <option key={n} value={n}>{n} files</option>)}
          </select>
        </label>
      </div>

      <Card className="mt-4 p-4">
        {loading && files.length === 0 ? (
          <p className="text-sm text-slate-500">Loading...</p>
        ) : files.length === 0 ? (
          <div className="py-6 text-center">
            <Clock className="mx-auto h-8 w-8 text-slate-300" />
            <p className="mt-2 text-sm font-semibold">No recent activity.</p>
            <p className="mt-1 text-[12px] text-slate-500">Run a sync or upload a file and it will show up here.</p>
          </div>
        ) : (
          <div className="grid gap-5">
            {grouped.map(([bucket, items]) => (
              <div key={bucket}>
                <p className="text-[11px] font-bold uppercase tracking-wide text-slate-400">{bucket} <span className="text-slate-300">({items.length})</span></p>
                <ul className="mt-2 grid gap-1">
                  {items.map((file) => (
                    <li key={file.id} className="flex flex-col gap-2 rounded-xl px-2 py-2 hover:bg-slate-50 sm:flex-row sm:items-center sm:justify-between">
                      <div className="flex min-w-0 flex-1 items-start gap-2.5">
                        <FileText className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" />
                        <div className="min-w-0">
                          <p className="flex items-center gap-2 truncate text-[13px] font-semibold" title={file.name}>
                            {file.name}
                            {file.starred ? <Star className="h-3.5 w-3.5 shrink-0 fill-amber-400 text-amber-400" /> : null}
                          </p>
                          <p className="mt-0.5 text-[11px] text-slate-500">
                            {formatBytes(file.sizeBytes)}
                            {file.accountEmail ? <> · {file.accountEmail}</> : null}
                            {file.folder?.name ? <> · {file.folder.name}</> : null}
                            {file.updatedAt ? <> · {formatDate(file.updatedAt)}</> : null}
                          </p>
                        </div>
                      </div>
                      <div className="flex shrink-0 gap-2">
                        {file.folder?.id ? (
                          <Button size="sm" variant="outline" onClick={() => { window.location.href = `/all-files?folderId=${file.folder?.id}` }}>
                            <FolderOpen className="h-4 w-4" />Folder
                          </Button>
                        ) : null}
                        <Button size="sm" variant="outline" onClick={() => toggleStar(file)} disabled={busyId === file.id}>
                          <Star className={file.starred ? 'h-4 w-4 fill-amber-400 text-amber-400' : 'h-4 w-4'} />
                        </Button>
                        <Button size="sm" variant="outline" onClick={() => download(file)}><Download className="h-4 w-4" />Download</Button>
                      </div>
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </div>
        )}
      </Card>
    </>
  )
}
