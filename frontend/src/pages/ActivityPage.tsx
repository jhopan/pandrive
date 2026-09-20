import { useCallback, useEffect, useState } from 'react'
import { Activity, ArrowRightLeft, CloudUpload, Download, LogIn, RefreshCw, Settings2, Trash2, Undo2, Upload, UserPlus, XCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, formatDate } from '@/lib/api'

type Entry = {
  id: string
  action: string
  targetType: string
  targetId: string
  targetName: string
  sizeBytes: string
  detail: string
  ip: string
  createdAt: string
  accountEmail: string
}

type Account = { id: string; email: string }

const ACTION_META: Record<string, { label: string; icon: typeof Activity; tone: string }> = {
  login: { label: 'Signed in', icon: LogIn, tone: 'text-emerald-600' },
  login_failed: { label: 'Failed sign-in', icon: XCircle, tone: 'text-red-600' },
  logout: { label: 'Signed out', icon: LogIn, tone: 'text-slate-500' },
  account_update: { label: 'Account updated', icon: UserPlus, tone: 'text-blue-600' },
  account_connect: { label: 'Account connected', icon: UserPlus, tone: 'text-emerald-600' },
  file_upload: { label: 'Uploaded', icon: Upload, tone: 'text-blue-600' },
  file_download: { label: 'Downloaded', icon: Download, tone: 'text-slate-600' },
  file_transfer: { label: 'Transferred', icon: ArrowRightLeft, tone: 'text-purple-600' },
  file_delete: { label: 'Trashed', icon: Trash2, tone: 'text-amber-600' },
  file_restore: { label: 'Restored', icon: Undo2, tone: 'text-emerald-600' },
  file_purge: { label: 'Permanently deleted', icon: Trash2, tone: 'text-red-600' },
  trash_empty: { label: 'Emptied Drive trash', icon: Trash2, tone: 'text-red-600' },
  sync_files: { label: 'Synced files', icon: RefreshCw, tone: 'text-slate-600' },
  oauth_config_add: { label: 'OAuth config added', icon: Settings2, tone: 'text-blue-600' },
  oauth_config_delete: { label: 'OAuth config deleted', icon: Settings2, tone: 'text-amber-600' },
}

export function ActivityPage() {
  const [entries, setEntries] = useState<Entry[]>([])
  const [actions, setActions] = useState<string[]>([])
  const [accounts, setAccounts] = useState<Account[]>([])
  const [action, setAction] = useState('all')
  const [accountId, setAccountId] = useState('all')
  const [total, setTotal] = useState(0)
  const [limit, setLimit] = useState(100)
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')

  const load = useCallback(async (nextLimit = limit) => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ limit: String(nextLimit), offset: '0' })
      if (action !== 'all') params.set('action', action)
      if (accountId !== 'all') params.set('accountId', accountId)
      const data = await apiFetch<{ entries: Entry[]; total: number; actions: string[] }>(`/activity?${params.toString()}`)
      setEntries(data.entries)
      setTotal(data.total)
      setActions(data.actions)
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load activity')
    } finally {
      setLoading(false)
    }
  }, [action, accountId, limit])

  useEffect(() => {
    load().catch(() => undefined)
  }, [load])

  useEffect(() => {
    apiFetch<{ accounts: Account[] }>('/connected-accounts')
      .then((data) => setAccounts(data.accounts))
      .catch(() => undefined)
  }, [])

  return (
    <>
      <PageHeader
        title="Activity"
        description="Audit trail: sign-ins, uploads, downloads, transfers and deletions."
        actions={
          <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
            <RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh
          </Button>
        }
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <div className="mt-5 flex flex-wrap items-end gap-3">
        <label className="grid gap-1.5 text-xs font-bold text-slate-500">
          Action
          <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={action} onChange={(e) => setAction(e.target.value)}>
            <option value="all">All actions</option>
            {actions.map((a) => <option key={a} value={a}>{ACTION_META[a]?.label ?? a}</option>)}
          </select>
        </label>
        <label className="grid gap-1.5 text-xs font-bold text-slate-500">
          Account
          <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={accountId} onChange={(e) => setAccountId(e.target.value)}>
            <option value="all">All accounts</option>
            {accounts.map((a) => <option key={a.id} value={a.id}>{a.email}</option>)}
          </select>
        </label>
        <span className="pb-2 text-[12px] text-slate-500">{total} entr{total === 1 ? 'y' : 'ies'}</span>
      </div>

      <Card className="mt-4 p-4">
        {loading && entries.length === 0 ? (
          <p className="text-sm text-slate-500">Loading...</p>
        ) : entries.length === 0 ? (
          <div className="py-6 text-center">
            <Activity className="mx-auto h-8 w-8 text-slate-300" />
            <p className="mt-2 text-sm font-semibold">No activity recorded yet.</p>
            <p className="mt-1 text-[12px] text-slate-500">Actions are logged here as you use PanDrive.</p>
          </div>
        ) : (
          <ul className="grid gap-1">
            {entries.map((entry) => {
              const meta = ACTION_META[entry.action] ?? { label: entry.action, icon: Activity, tone: 'text-slate-500' }
              const Icon = meta.icon
              return (
                <li key={entry.id} className="flex items-start gap-3 rounded-xl px-2 py-2.5 hover:bg-slate-50">
                  <Icon className={`mt-0.5 h-4 w-4 shrink-0 ${meta.tone}`} />
                  <div className="min-w-0 flex-1">
                    <p className="text-[13px]">
                      <span className="font-semibold">{meta.label}</span>
                      {entry.targetName ? <span className="text-slate-600"> · {entry.targetName}</span> : null}
                      {entry.sizeBytes && entry.sizeBytes !== '0' ? <span className="text-slate-400"> · {formatBytes(entry.sizeBytes)}</span> : null}
                    </p>
                    <p className="mt-0.5 text-[11px] text-slate-500">
                      {entry.createdAt ? formatDate(entry.createdAt) : ''}
                      {entry.accountEmail ? <> · {entry.accountEmail}</> : null}
                      {entry.detail ? <> · {entry.detail}</> : null}
                      {entry.ip ? <> · {entry.ip}</> : null}
                    </p>
                  </div>
                </li>
              )
            })}
          </ul>
        )}

        {entries.length < total ? (
          <div className="mt-4 flex justify-center">
            <Button variant="outline" size="sm" disabled={loading} onClick={() => { const next = limit + 100; setLimit(next); load(next).catch(() => undefined) }}>
              <CloudUpload className="h-4 w-4" />Load more
            </Button>
          </div>
        ) : null}
      </Card>
    </>
  )
}
