import { useEffect, useState } from 'react'
import { BarChart3, HardDrive, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, formatDate } from '@/lib/api'

type AccountRow = { id: string; email: string; fileCount: number; totalBytes: string; usedBytes: string; availableBytes: string }
type TypeRow = { label: string; bytes: string; count: number }
type LargestFile = { id: string; name: string; sizeBytes: string; mimeType: string; accountEmail: string; folder: string; createdAt: string }
type Analyzer = { accounts: AccountRow[]; byType: TypeRow[]; largest: LargestFile[]; totals: { files: string; bytes: string } }

const TYPE_COLORS: Record<string, string> = {
  Video: 'bg-rose-500',
  Images: 'bg-blue-500',
  Documents: 'bg-cyan-500',
  Archives: 'bg-amber-500',
  Audio: 'bg-purple-500',
  Installers: 'bg-orange-500',
  Code: 'bg-emerald-500',
  Other: 'bg-slate-400',
}

export function StoragePage() {
  const [data, setData] = useState<Analyzer | null>(null)
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')

  async function load() {
    setLoading(true)
    try {
      setData(await apiFetch<Analyzer>('/storage/analyzer'))
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to analyse storage')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load().catch(() => undefined)
  }, [])

  const totalBytes = Number(data?.totals.bytes ?? 0)
  const maxAccount = Math.max(1, ...(data?.accounts ?? []).map((a) => Number(a.totalBytes)))

  return (
    <>
      <PageHeader
        title="Storage"
        description="Where your space actually goes: per account, by file type, and the biggest files."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <div className="mt-5 grid gap-4 sm:grid-cols-3">
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Synced files</p><p className="mt-1 text-2xl font-extrabold">{data?.totals.files ?? 0}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Indexed size</p><p className="mt-1 text-2xl font-extrabold">{formatBytes(totalBytes)}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Accounts</p><p className="mt-1 text-2xl font-extrabold">{data?.accounts.length ?? 0}</p></Card>
      </div>

      <div className="mt-5 grid gap-4 lg:grid-cols-2">
        <Card className="p-4">
          <h2 className="flex items-center gap-2 text-[16px] font-bold"><HardDrive className="h-5 w-5 text-blue-500" />Per account</h2>
          <ul className="mt-3 grid gap-3">
            {(data?.accounts ?? []).map((account) => {
              const pct = totalBytes > 0 ? (Number(account.totalBytes) / totalBytes) * 100 : 0
              const width = (Number(account.totalBytes) / maxAccount) * 100
              return (
                <li key={account.id}>
                  <div className="flex items-center justify-between text-[13px]">
                    <span className="truncate font-semibold" title={account.email}>{account.email}</span>
                    <span className="shrink-0 text-slate-500">{formatBytes(account.totalBytes)} · {pct.toFixed(0)}%</span>
                  </div>
                  <div className="mt-1.5 h-2 overflow-hidden rounded-full bg-slate-100">
                    <div className="h-full rounded-full bg-blue-500" style={{ width: `${width}%` }} />
                  </div>
                  <p className="mt-1 text-[11px] text-slate-500">
                    {account.fileCount} files · drive {formatBytes(account.usedBytes)} used / {formatBytes(account.availableBytes)} free
                  </p>
                </li>
              )
            })}
            {data && data.accounts.length === 0 ? <li className="text-sm text-slate-500">No connected accounts.</li> : null}
          </ul>
        </Card>

        <Card className="p-4">
          <h2 className="flex items-center gap-2 text-[16px] font-bold"><BarChart3 className="h-5 w-5 text-cyan-500" />By file type</h2>
          <ul className="mt-3 grid gap-2.5">
            {(data?.byType ?? []).map((row) => {
              const pct = totalBytes > 0 ? (Number(row.bytes) / totalBytes) * 100 : 0
              return (
                <li key={row.label}>
                  <div className="flex items-center justify-between text-[13px]">
                    <span className="flex items-center gap-2 font-semibold">
                      <span className={`h-2.5 w-2.5 rounded-full ${TYPE_COLORS[row.label] ?? 'bg-slate-400'}`} />
                      {row.label}
                    </span>
                    <span className="text-slate-500">{formatBytes(row.bytes)} · {row.count} · {pct.toFixed(0)}%</span>
                  </div>
                  <div className="mt-1.5 h-2 overflow-hidden rounded-full bg-slate-100">
                    <div className={`h-full rounded-full ${TYPE_COLORS[row.label] ?? 'bg-slate-400'}`} style={{ width: `${pct}%` }} />
                  </div>
                </li>
              )
            })}
            {data && data.byType.length === 0 ? <li className="text-sm text-slate-500">Nothing indexed yet. Run a sync first.</li> : null}
          </ul>
        </Card>
      </div>

      <Card className="mt-4 p-4">
        <h2 className="text-[16px] font-bold">Largest files</h2>
        <div className="mt-3 overflow-x-auto">
          <table className="w-full text-left text-[13px]">
            <thead>
              <tr className="border-b border-slate-100 text-[11px] uppercase text-slate-400">
                <th className="py-2 pr-3 font-bold">Name</th>
                <th className="py-2 pr-3 font-bold">Size</th>
                <th className="py-2 pr-3 font-bold">Account</th>
                <th className="py-2 pr-3 font-bold">Folder</th>
                <th className="py-2 font-bold">Added</th>
              </tr>
            </thead>
            <tbody>
              {(data?.largest ?? []).map((file) => (
                <tr key={file.id} className="border-b border-slate-50 hover:bg-slate-50">
                  <td className="max-w-[280px] truncate py-2 pr-3 font-medium" title={file.name}>{file.name}</td>
                  <td className="py-2 pr-3 text-slate-500">{formatBytes(file.sizeBytes)}</td>
                  <td className="max-w-[200px] truncate py-2 pr-3 text-slate-500">{file.accountEmail}</td>
                  <td className="py-2 pr-3 text-slate-500">{file.folder || '-'}</td>
                  <td className="py-2 text-slate-500">{file.createdAt ? formatDate(file.createdAt) : '-'}</td>
                </tr>
              ))}
              {data && data.largest.length === 0 ? <tr><td colSpan={5} className="py-3 text-slate-500">No files indexed yet.</td></tr> : null}
            </tbody>
          </table>
        </div>
      </Card>
    </>
  )
}
