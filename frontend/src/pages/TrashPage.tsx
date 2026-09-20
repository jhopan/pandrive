import { useEffect, useState } from 'react'
import { AlertTriangle, RefreshCw, RotateCcw, Trash2, Undo2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, formatDate } from '@/lib/api'

type TrashFile = {
  id: string
  name: string
  mimeType: string
  sizeBytes: string
  deletedAt?: string
  createdAt: string
  connectedAccount?: { id: string; email: string; provider: string }
}

type Account = { id: string; email: string; availableBytes?: string }

export function TrashPage() {
  const [files, setFiles] = useState<TrashFile[]>([])
  const [accounts, setAccounts] = useState<Account[]>([])
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [emptyingId, setEmptyingId] = useState<string | null>(null)
  const [message, setMessage] = useState('')
  const [confirmEmpty, setConfirmEmpty] = useState<Account | null>(null)

  async function load() {
    setLoading(true)
    try {
      const [trash, acct] = await Promise.all([
        apiFetch<{ files: TrashFile[] }>('/files?status=deleted'),
        apiFetch<{ accounts: { id: string; email: string; storageAccount?: { availableBytes: string } | null }[] }>('/connected-accounts'),
      ])
      setFiles(trash.files)
      setAccounts(acct.accounts.map((a) => ({ id: a.id, email: a.email, availableBytes: a.storageAccount?.availableBytes })))
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load trash')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load().catch(() => undefined)
  }, [])

  async function restore(file: TrashFile) {
    setBusyId(file.id)
    setMessage('')
    try {
      await apiFetch(`/files/${file.id}/restore`, { method: 'POST' })
      setFiles((prev) => prev.filter((f) => f.id !== file.id))
      setMessage(`"${file.name}" restored.`)
      window.dispatchEvent(new Event('pandrive:storage-changed'))
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Restore failed')
    } finally {
      setBusyId(null)
    }
  }

  async function purge(file: TrashFile) {
    if (!confirm(`Permanently delete "${file.name}" from Google Drive? This cannot be undone.`)) return
    setBusyId(file.id)
    setMessage('')
    try {
      await apiFetch(`/files/${file.id}/purge`, { method: 'POST' })
      setFiles((prev) => prev.filter((f) => f.id !== file.id))
      setMessage(`"${file.name}" permanently deleted.`)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Permanent delete failed')
    } finally {
      setBusyId(null)
    }
  }

  async function emptyTrash(account: Account) {
    setEmptyingId(account.id)
    setMessage('')
    try {
      await apiFetch(`/connected-accounts/${account.id}/empty-trash`, { method: 'POST' })
      setMessage(`Trash emptied for ${account.email}. Quota freed.`)
      await load()
      window.dispatchEvent(new Event('pandrive:storage-changed'))
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Empty trash failed')
    } finally {
      setEmptyingId(null)
      setConfirmEmpty(null)
    }
  }

  const totalSize = files.reduce((sum, f) => sum + Number(f.sizeBytes || 0), 0)

  return (
    <>
      <PageHeader
        title="Trash"
        description="Files hidden from your library. Deleting here removes them from Google Drive too."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-blue-50 p-3 text-sm text-blue-700">{message}</p> : null}

      <div className="mt-5 grid gap-4 lg:grid-cols-[1fr_300px]">
        <Card className="p-4">
          <div className="flex items-center justify-between border-b border-slate-100 pb-3">
            <h2 className="text-[16px] font-bold">Deleted files</h2>
            <span className="text-[12px] text-slate-500">{files.length} item{files.length === 1 ? '' : 's'} · {formatBytes(totalSize)}</span>
          </div>

          {loading ? (
            <p className="mt-4 text-sm text-slate-500">Loading...</p>
          ) : files.length === 0 ? (
            <p className="mt-4 text-sm text-slate-500">Trash is empty.</p>
          ) : (
            <ul className="mt-3 grid gap-2">
              {files.map((file) => (
                <li key={file.id} className="flex flex-col gap-2 rounded-xl border border-slate-100 p-3 sm:flex-row sm:items-center sm:justify-between">
                  <div className="min-w-0">
                    <p className="truncate text-sm font-semibold" title={file.name}>{file.name}</p>
                    <p className="mt-0.5 text-[11px] text-slate-500">
                      {formatBytes(file.sizeBytes)} · {file.connectedAccount?.email ?? 'unknown account'}
                      {file.deletedAt ? <> · deleted {formatDate(file.deletedAt)}</> : null}
                    </p>
                  </div>
                  <div className="flex shrink-0 gap-2">
                    <Button size="sm" variant="outline" onClick={() => restore(file)} disabled={busyId === file.id}>
                      <Undo2 className="h-4 w-4" />Restore
                    </Button>
                    <Button size="sm" variant="danger" onClick={() => purge(file)} disabled={busyId === file.id}>
                      <Trash2 className="h-4 w-4" />Delete
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <div className="grid gap-4">
          <Card className="p-4">
            <div className="flex items-center gap-2">
              <AlertTriangle className="h-5 w-5 text-amber-500" />
              <h2 className="text-[15px] font-bold">Drive trash per account</h2>
            </div>
            <p className="mt-1.5 text-[12px] leading-relaxed text-slate-500">
              Files deleted in Google Drive still count toward quota until the Drive trash is emptied. Moving files between accounts also lands the old copy here.
            </p>
            <ul className="mt-3 grid gap-2">
              {accounts.map((account) => (
                <li key={account.id} className="rounded-xl border border-slate-100 p-3">
                  <p className="truncate text-[13px] font-semibold">{account.email}</p>
                  <p className="mt-0.5 text-[11px] text-slate-500">Free: {account.availableBytes ? formatBytes(account.availableBytes) : 'unknown'}</p>
                  <Button className="mt-2 w-full" size="sm" variant="outline" onClick={() => setConfirmEmpty(account)} disabled={emptyingId === account.id}>
                    <RotateCcw className={emptyingId === account.id ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />
                    {emptyingId === account.id ? 'Emptying...' : 'Empty Drive trash'}
                  </Button>
                </li>
              ))}
              {accounts.length === 0 ? <li className="text-sm text-slate-500">No connected accounts.</li> : null}
            </ul>
          </Card>
        </div>
      </div>

      {confirmEmpty ? (
        <div className="fixed inset-0 z-[80] grid place-items-center bg-slate-950/50 p-4">
          <Card className="w-full max-w-sm p-4">
            <h3 className="text-[15px] font-bold">Empty Drive trash?</h3>
            <p className="mt-2 text-[13px] text-slate-600">
              This permanently deletes everything in <span className="font-semibold">{confirmEmpty.email}</span> Drive trash. It cannot be undone, but it frees the quota those files still occupy.
            </p>
            <div className="mt-4 flex justify-end gap-2">
              <Button variant="outline" size="sm" onClick={() => setConfirmEmpty(null)}>Cancel</Button>
              <Button variant="danger" size="sm" onClick={() => emptyTrash(confirmEmpty)}>Empty trash</Button>
            </div>
          </Card>
        </div>
      ) : null}
    </>
  )
}
