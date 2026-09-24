import { useCallback, useEffect, useState } from 'react'
import { apiFetch } from '@/lib/api'
import { PageHeader } from '@/components/drive/PageHeader'
import { Button } from '@/components/ui/button'
import { ArrowLeftRight, RefreshCw } from 'lucide-react'

type Account = { id: string; email: string; status: string }
type Move = { fileId: string; name: string; sizeBytes: string }
type Plan = {
  sourceAccountId: string
  targetAccountId: string
  sourceUsedBytes: string
  sourceTotalBytes: string
  targetFreeBytes: string
  targetPercent: number
  moves: Move[]
  movedBytes: string
  resultUsedBytes: string
  error?: string
}
type ExecResult = { results: Array<{ fileId: string; name: string; ok: boolean; error?: string }>; moved: number; failed: number }

function fmtBytes(v: string | number) {
  let b = typeof v === 'string' ? parseFloat(v) : v
  if (!+b) return '0 B'
  const u = ['B','KB','MB','GB','TB']
  let i = 0
  while (b >= 1024 && i < u.length-1) { b /= 1024; i++ }
  return `${b.toFixed(2)} ${u[i]}`
}

export function RebalancePage() {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [source, setSource] = useState('')
  const [percent, setPercent] = useState('80')
  const [plan, setPlan] = useState<Plan | null>(null)
  const [running, setRunning] = useState(false)
  const [result, setResult] = useState<ExecResult | null>(null)
  const [message, setMessage] = useState('')

  const load = useCallback(async () => {
    try {
      const data = await apiFetch<{ accounts: Account[] }>('/connected-accounts')
      setAccounts((data.accounts || []).filter((a: Account) => a.status === 'connected'))
    } catch { setMessage('Gagal memuat akun') }
  }, [])

  useEffect(() => { load() }, [load])

  async function analyze() {
    setPlan(null); setResult(null); setMessage('')
    try {
      const data = await apiFetch<Plan>('/rebalance/analyze', {
        method: 'POST',
        body: JSON.stringify({ sourceAccountId: source, targetPercent: parseFloat(percent) || 80 })
      })
      setPlan(data)
      if (data.error) setMessage(data.error)
    } catch (err) {
      setMessage((err as Error).message || 'Gagal menganalisis')
    }
  }

  async function execute() {
    if (!plan?.moves?.length) return
    setRunning(true); setMessage('')
    try {
      const data = await apiFetch<ExecResult>('/rebalance/execute', {
        method: 'POST',
        body: JSON.stringify({
          sourceAccountId: plan.sourceAccountId,
          targetAccountId: plan.targetAccountId,
          fileIds: plan.moves.map(m => m.fileId)
        })
      })
      setResult(data)
      setMessage(`${data.moved} dipindah, ${data.failed} gagal.`)
      setPlan(null)
    } catch (err) {
      setMessage((err as Error).message || 'Gagal menjalankan rebalance')
    } finally {
      setRunning(false)
    }
  }

  return (
    <div className="mx-auto w-full max-w-5xl space-y-6 px-4 pb-16 pt-6">
      <PageHeader
        title="Smart Rebalancing"
        description="Pindahkan file dari akun yang penuh ke akun yang kosong, otomatis — server-side, tanpa bandwidth."
        actions={<Button variant="outline" size="sm" onClick={load}><RefreshCw className="h-4 w-4" />Refresh</Button>}
      />

      <div className="rounded-2xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900/60">
        <div className="grid gap-4 sm:grid-cols-[1fr_140px_auto] sm:items-end">
          <label className="block text-sm">
            <span className="mb-1.5 block font-semibold text-slate-700 dark:text-slate-200">Akun sumber</span>
            <select value={source} onChange={(e) => setSource(e.target.value)} className="h-10 w-full rounded-lg border border-slate-300 bg-transparent px-3 text-sm dark:border-slate-700">
              <option value="">Pilih akun…</option>
              {accounts.map(a => <option key={a.id} value={a.id}>{a.email}</option>)}
            </select>
          </label>
          <label className="block text-sm">
            <span className="mb-1.5 block font-semibold text-slate-700 dark:text-slate-200">Target sisa (%)</span>
            <input type="number" min={1} max={100} value={percent} onChange={(e) => setPercent(e.target.value)} className="h-10 w-full rounded-lg border border-slate-300 bg-transparent px-3 text-sm dark:border-slate-700" />
          </label>
          <Button onClick={analyze} disabled={!source}><ArrowLeftRight className="h-4 w-4" />Analisis</Button>
        </div>
      </div>

      {message && <p className="text-sm text-slate-500">{message}</p>}

      {plan && !plan.error && (
        <div className="rounded-2xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900/60">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
            <h2 className="text-sm font-extrabold uppercase tracking-wide text-slate-500">Rencana (dry-run)</h2>
            <Button onClick={execute} disabled={running || !plan.moves?.length}>
              {running ? 'Memindah…' : `Jalankan (${plan.moves?.length ?? 0} file)`}
            </Button>
          </div>
          <div className="grid gap-2 text-sm text-slate-600 dark:text-slate-300 sm:grid-cols-3">
            <p>Sumber terpakai: <b>{fmtBytes(plan.sourceUsedBytes)}</b> / {fmtBytes(plan.sourceTotalBytes)}</p>
            <p>Tujuan: <b>{plan.targetAccountId}</b> (bebas {fmtBytes(plan.targetFreeBytes)})</p>
            <p>Hasil sumber: <b>{fmtBytes(plan.resultUsedBytes)}</b></p>
          </div>
          <ul className="mt-4 divide-y divide-slate-100 dark:divide-slate-800">
            {(plan.moves || []).map(m => (
              <li key={m.fileId} className="flex items-center justify-between gap-3 py-2 text-sm">
                <span className="min-w-0 flex-1 truncate">{m.name}</span>
                <span className="shrink-0 text-slate-500">{fmtBytes(m.sizeBytes)}</span>
              </li>
            ))}
            {!plan.moves?.length && <li className="py-2 text-sm text-slate-500">Tidak ada yang perlu dipindah.</li>}
          </ul>
        </div>
      )}

      {result && (
        <div className="rounded-2xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900/60">
          <h2 className="mb-3 text-sm font-extrabold uppercase tracking-wide text-slate-500">Hasil eksekusi</h2>
          <ul className="divide-y divide-slate-100 dark:divide-slate-800">
            {result.results.map(rr => (
              <li key={rr.fileId} className="flex items-center justify-between gap-3 py-2 text-sm">
                <span className="min-w-0 flex-1 truncate">{rr.name || rr.fileId}</span>
                <span className={rr.ok ? 'text-emerald-600' : 'text-red-500'}>{rr.ok ? 'OK' : (rr.error || 'gagal')}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}
