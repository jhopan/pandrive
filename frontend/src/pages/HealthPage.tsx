import { useEffect, useState } from 'react'
import { AlertTriangle, CheckCircle2, Database, HardDrive, RefreshCw, ShieldCheck, Wifi } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes } from '@/lib/api'

type AccountHealth = {
  id: string
  email: string
  status: string
  lastError: string
  tokenExpiresAt: string
  tokenExpiresInSeconds: number
  fileCount: number
  usedBytes: string
  availableBytes: string
  lastSyncedAt: string
  syncAgeSeconds: number
}
type ConfigHealth = { id: string; label: string; status: string; requestCount: number; windowStart: string; lastUsedAt: string }
type Health = {
  version: string
  startedAt: string
  uptimeSeconds: number
  database: { path: string; sizeBytes: string; writable: boolean; backupPath: string; backupExists: boolean; backupAgeSeconds: number }
  tunnel: { mode: string; enabled: boolean }
  sync: { intervalMinutes: number; quotaThreshold: number; quotaWindowMax: number }
  accounts: AccountHealth[]
  oauthConfigs: ConfigHealth[]
  totals: { files: number; bytes: string; trashedFiles: number }
}

function human(seconds: number): string {
  if (seconds < 0) return 'never'
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

export function HealthPage() {
  const [data, setData] = useState<Health | null>(null)
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')

  async function load() {
    setLoading(true)
    try {
      setData(await apiFetch<Health>('/system/health'))
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load health')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load().catch(() => undefined)
  }, [])

  const backupStale = (data?.database.backupAgeSeconds ?? 0) > 26 * 3600
  const allGood = (data?.accounts ?? []).every((a) => a.status === 'connected' && !a.lastError)

  return (
    <>
      <PageHeader
        title="Health"
        description="Runtime state: uptime, database, backup freshness, tunnel, tokens and sync."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <div className={`mt-5 flex items-center gap-3 rounded-2xl p-4 ${allGood ? 'bg-emerald-50' : 'bg-amber-50'}`}>
        {allGood ? <CheckCircle2 className="h-6 w-6 text-emerald-600" /> : <AlertTriangle className="h-6 w-6 text-amber-600" />}
        <div>
          <p className="text-sm font-bold">{allGood ? 'All connected accounts healthy' : 'Attention needed'}</p>
          <p className="text-[12px] text-slate-600">
            {data ? `${data.accounts.length} account(s) · ${data.oauthConfigs.length} OAuth config(s) · ${data.totals.files} files indexed` : 'Loading...'}
          </p>
        </div>
      </div>

      <div className="mt-4 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Card className="p-4">
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><Database className="h-4 w-4" />Runtime</p>
          <p className="mt-2 text-[13px] font-semibold">{data?.version ?? '-'}</p>
          <p className="mt-0.5 text-[11px] text-slate-500">up {human(data?.uptimeSeconds ?? 0).replace(' ago', '')}</p>
          <p className="mt-0.5 text-[11px] text-slate-500">sync every {data?.sync.intervalMinutes ?? 5} min</p>
        </Card>
        <Card className="p-4">
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><HardDrive className="h-4 w-4" />Database</p>
          <p className="mt-2 text-[13px] font-semibold">{data ? formatBytes(data.database.sizeBytes) : '-'}</p>
          <p className="mt-0.5 text-[11px] text-slate-500">{data?.database.writable ? 'writable' : 'read-only'}</p>
        </Card>
        <Card className={`p-4 ${backupStale ? 'ring-1 ring-amber-300' : ''}`}>
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><ShieldCheck className="h-4 w-4" />Backup</p>
          <p className="mt-2 text-[13px] font-semibold">{data?.database.backupExists ? human(data.database.backupAgeSeconds) : 'missing'}</p>
          <p className="mt-0.5 text-[11px] text-slate-500">{backupStale ? 'older than 26h' : 'fresh (daily)'}</p>
        </Card>
        <Card className="p-4">
          <p className="flex items-center gap-2 text-xs font-bold uppercase text-slate-400"><Wifi className="h-4 w-4" />Tunnel</p>
          <p className="mt-2 text-[13px] font-semibold">{data?.tunnel.enabled ? data.tunnel.mode : 'off'}</p>
          <p className="mt-0.5 text-[11px] text-slate-500">{formatBytes(data?.totals.bytes ?? 0)} indexed</p>
        </Card>
      </div>

      <Card className="mt-4 p-4">
        <h2 className="text-[16px] font-bold">Connected accounts</h2>
        <div className="mt-3 grid gap-3">
          {(data?.accounts ?? []).map((account) => {
            const tokenSoon = account.tokenExpiresInSeconds >= 0 && account.tokenExpiresInSeconds < 300
            const syncStale = account.syncAgeSeconds < 0 || account.syncAgeSeconds > 900
            return (
              <div key={account.id} className="rounded-xl border border-slate-100 p-3">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <p className="truncate text-[13px] font-semibold">{account.email}</p>
                  <span className={`rounded-full px-2 py-0.5 text-[10px] font-bold uppercase ${account.status === 'connected' ? 'bg-emerald-100 text-emerald-700' : 'bg-red-100 text-red-700'}`}>{account.status}</span>
                </div>
                <div className="mt-2 grid gap-1 text-[11px] text-slate-500 sm:grid-cols-2 lg:grid-cols-4">
                  <span>Token: {account.tokenExpiresInSeconds < 0 ? 'no expiry stored' : tokenSoon ? 'expiring soon (auto-refresh)' : `valid ${human(0).replace(' ago', '')}${Math.floor(account.tokenExpiresInSeconds / 60)}m`}</span>
                  <span className={syncStale ? 'text-amber-600' : ''}>Quota sync: {human(account.syncAgeSeconds)}</span>
                  <span>{account.fileCount} files</span>
                  <span>{formatBytes(account.usedBytes)} used / {formatBytes(account.availableBytes)} free</span>
                </div>
                {account.lastError ? <p className="mt-1.5 text-[11px] text-red-600">{account.lastError}</p> : null}
              </div>
            )
          })}
          {data && data.accounts.length === 0 ? <p className="text-sm text-slate-500">No accounts connected.</p> : null}
        </div>
      </Card>

      <Card className="mt-4 p-4">
        <h2 className="text-[16px] font-bold">OAuth configs (rotation)</h2>
        <p className="mt-1 text-[12px] text-slate-500">Auto-switch happens at {data?.sync.quotaThreshold ?? 8000} requests per {data?.sync.quotaWindowMax ?? 10000} limit window.</p>
        <div className="mt-3 overflow-x-auto">
          <table className="w-full text-left text-[13px]">
            <thead>
              <tr className="border-b border-slate-100 text-[11px] uppercase text-slate-400">
                <th className="py-2 pr-3 font-bold">Label</th>
                <th className="py-2 pr-3 font-bold">Status</th>
                <th className="py-2 pr-3 font-bold">Requests in window</th>
                <th className="py-2 pr-3 font-bold">Last used</th>
              </tr>
            </thead>
            <tbody>
              {(data?.oauthConfigs ?? []).map((config) => {
                const pct = Math.min(100, (config.requestCount / (data?.sync.quotaWindowMax ?? 10000)) * 100)
                return (
                  <tr key={config.id} className="border-b border-slate-50">
                    <td className="py-2 pr-3 font-medium">{config.label || config.id.slice(0, 12)}</td>
                    <td className="py-2 pr-3"><span className={config.status === 'active' ? 'text-emerald-600' : 'text-slate-400'}>{config.status}</span></td>
                    <td className="py-2 pr-3">
                      <div className="flex items-center gap-2">
                        <div className="h-2 w-28 overflow-hidden rounded-full bg-slate-100">
                          <div className={`h-full rounded-full ${pct > 80 ? 'bg-amber-500' : 'bg-blue-500'}`} style={{ width: `${pct}%` }} />
                        </div>
                        <span className="text-slate-500">{config.requestCount}</span>
                      </div>
                    </td>
                    <td className="py-2 pr-3 text-slate-500">{config.lastUsedAt || 'never'}</td>
                  </tr>
                )
              })}
              {data && data.oauthConfigs.length === 0 ? <tr><td colSpan={4} className="py-3 text-slate-500">No OAuth configs saved.</td></tr> : null}
            </tbody>
          </table>
        </div>
      </Card>
    </>
  )
}
