import { useEffect, useState } from 'react'
import { ArrowRightLeft, HardDrive } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { DummyModal } from '@/components/drive/DummyModal'
import { apiFetch, formatBytes } from '@/lib/api'

type Account = { id: string; email: string; storageAccount?: { availableBytes: string | null } | null }
type FileLite = { id: string; name: string; sizeBytes?: string; accountEmail?: string }

// Transfer moves (or copies) files between two connected Drive accounts server-side:
// no bytes pass through the browser or this server, only Google does the copying.
export function TransferModal({ open, files, onClose, onDone }: { open: boolean; files: FileLite[]; onClose: () => void; onDone?: () => void }) {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [target, setTarget] = useState('')
  const [deleteSource, setDeleteSource] = useState(true)
  const [running, setRunning] = useState(false)
  const [results, setResults] = useState<{ ok: number; failed: { name: string; error: string }[] } | null>(null)

  useEffect(() => {
    if (!open) return
    setResults(null)
    apiFetch<{ accounts: Account[] }>('/connected-accounts')
      .then((data) => {
        setAccounts(data.accounts)
        setTarget((prev) => prev || data.accounts[0]?.id || '')
      })
      .catch(() => undefined)
  }, [open])

  async function run() {
    if (!target || files.length === 0) return
    setRunning(true)
    setResults(null)
    let ok = 0
    const failed: { name: string; error: string }[] = []
    for (const file of files) {
      try {
        await apiFetch(`/files/${file.id}/transfer`, {
          method: 'POST',
          body: JSON.stringify({ targetAccountId: target, deleteSource }),
        })
        ok++
      } catch (error) {
        failed.push({ name: file.name, error: error instanceof Error ? error.message : 'failed' })
      }
    }
    setRunning(false)
    setResults({ ok, failed })
    window.dispatchEvent(new Event('pandrive:storage-changed'))
    if (failed.length === 0) onDone?.()
  }

  const totalSize = files.reduce((sum, f) => sum + Number(f.sizeBytes || 0), 0)
  const targetAccount = accounts.find((a) => a.id === target)

  return (
    <DummyModal
      open={open}
      onClose={() => { if (!running) onClose() }}
      title="Transfer between accounts"
      description="Google copies the file server-side. Your bandwidth is not used."
      className="max-w-lg"
    >
      <div className="grid gap-4 py-1">
        <div className="rounded-xl bg-slate-50 p-3 text-[13px]">
          <p className="font-semibold">{files.length} file{files.length === 1 ? '' : 's'} selected · {formatBytes(totalSize)}</p>
          <ul className="mt-1.5 max-h-28 overflow-y-auto text-[12px] text-slate-600">
            {files.slice(0, 50).map((f) => <li key={f.id} className="truncate">• {f.name}</li>)}
            {files.length > 50 ? <li>… and {files.length - 50} more</li> : null}
          </ul>
        </div>

        <label className="grid gap-1.5 text-xs font-bold text-slate-500">
          Destination account
          <select
            className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal text-slate-900"
            value={target}
            onChange={(e) => setTarget(e.target.value)}
          >
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>{a.email}{a.storageAccount?.availableBytes ? ` · ${formatBytes(a.storageAccount.availableBytes)} free` : ''}</option>
            ))}
          </select>
        </label>

        {targetAccount ? (
          <p className="flex items-center gap-2 text-[12px] text-slate-500">
            <HardDrive className="h-4 w-4" />
            Needs at least {formatBytes(totalSize)} free on {targetAccount.email} during the copy.
          </p>
        ) : null}

        <label className="flex items-start gap-2.5 rounded-xl border border-slate-200 p-3 text-[13px]">
          <input type="checkbox" className="mt-0.5" checked={deleteSource} onChange={(e) => setDeleteSource(e.target.checked)} />
          <span>
            <span className="font-semibold">Move (delete the source copy)</span>
            <span className="mt-0.5 block text-[12px] text-slate-500">
              The original goes to that account&apos;s Drive trash. Empty the Drive trash (Trash menu) to actually free quota.
            </span>
          </span>
        </label>

        {results ? (
          <div className="rounded-xl bg-slate-50 p-3 text-[13px]">
            <p className="font-semibold text-emerald-700">{results.ok} transferred</p>
            {results.failed.length > 0 ? (
              <ul className="mt-1.5 max-h-28 overflow-y-auto text-[12px] text-red-600">
                {results.failed.map((f) => <li key={f.name} className="truncate">• {f.name}: {f.error}</li>)}
              </ul>
            ) : null}
          </div>
        ) : null}

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={onClose} disabled={running}>Close</Button>
          <Button onClick={run} disabled={running || !target || files.length === 0}>
            <ArrowRightLeft className={running ? 'h-4 w-4 animate-pulse' : 'h-4 w-4'} />
            {running ? 'Transferring...' : deleteSource ? 'Move files' : 'Copy files'}
          </Button>
        </div>
      </div>
    </DummyModal>
  )
}
