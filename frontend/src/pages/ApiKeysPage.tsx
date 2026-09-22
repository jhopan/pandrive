import { useCallback, useEffect, useState } from 'react'
import { Copy, KeyRound, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { Input } from '@/components/ui/input'
import { apiFetch, formatDate } from '@/lib/api'

type ApiKey = {
  id: string
  name: string
  prefix: string
  scopes: string
  lastUsedAt: string
  revoked: boolean
  createdAt: string
}

// Programmatic access tokens (pd_...) for Android / scripts. The plaintext is shown exactly once.
export function ApiKeysPage() {
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [loading, setLoading] = useState(true)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [freshKey, setFreshKey] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const d = await apiFetch<{ keys: ApiKey[] }>('/keys')
      setKeys(d.keys)
    } catch (e) { setMessage(e instanceof Error ? e.message : 'Gagal memuat') } finally { setLoading(false) }
  }, [])

  useEffect(() => { load().catch(() => undefined) }, [load])

  async function create() {
    setBusy(true); setMessage('')
    try {
      const d = await apiFetch<{ key: string }>('/keys', { method: 'POST', body: JSON.stringify({ name: name || 'API key' }) })
      setFreshKey(d.key)
      setName('')
      await load()
    } catch (e) { setMessage(e instanceof Error ? e.message : 'Gagal membuat key') } finally { setBusy(false) }
  }

  async function revoke(id: string) {
    if (!confirm('Cabut API key ini? Aplikasi yang memakainya langsung kehilangan akses.')) return
    try { await apiFetch('/keys/' + id, { method: 'DELETE' }); await load() }
    catch (e) { setMessage(e instanceof Error ? e.message : 'Gagal revoke') }
  }

  async function hardDelete(id: string) {
    if (!confirm('Hapus permanen dari daftar?')) return
    try { await apiFetch('/keys/' + id + '/hard', { method: 'DELETE' }); await load() }
    catch (e) { setMessage(e instanceof Error ? e.message : 'Gagal hapus') }
  }

  return (
    <div className="mx-auto w-full max-w-3xl px-4 py-8 sm:px-6">
      <PageHeader
        title="API Keys"
        description="Token akses programatik untuk Android/script. Key hanya ditampilkan sekali saat dibuat."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={`h-4 w-4 ${loading ? 'animate-spin' : ''}`} />Refresh</Button>}
      />
      {message ? <Card className="mt-4 border-blue-200 bg-blue-50 p-3 text-[13px] font-semibold text-blue-700 dark:border-blue-500/30 dark:bg-blue-500/10 dark:text-blue-300">{message}</Card> : null}

      {freshKey ? (
        <Card className="mt-4 border-emerald-200 bg-emerald-50 p-4 dark:border-emerald-500/30 dark:bg-emerald-500/10">
          <p className="text-[13px] font-bold text-emerald-800 dark:text-emerald-300">Key baru (salin sekarang, hanya tampil sekali):</p>
          <div className="mt-2 flex gap-2">
            <Input readOnly value={freshKey} onFocus={(e) => e.currentTarget.select()} />
            <Button size="sm" onClick={() => { navigator.clipboard.writeText(freshKey); setMessage('Key tersalin ke clipboard.') }}><Copy className="h-4 w-4" />Copy</Button>
          </div>
        </Card>
      ) : null}

      <Card className="mt-4 p-4">
        <h2 className="text-[14px] font-bold">Buat key baru</h2>
        <div className="mt-2 flex gap-2">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Nama (mis. Android HP)" maxLength={64} />
          <Button disabled={busy} onClick={create}><Plus className="h-4 w-4" />{busy ? 'Membuat...' : 'Create'}</Button>
        </div>
        <p className="mt-2 text-[11px] text-slate-500">Maksimal 10 key aktif. Pakai header: <code className="rounded bg-slate-100 px-1 dark:bg-slate-800">Authorization: Bearer pd_...</code></p>
      </Card>

      <Card className="mt-4 p-4">
        <h2 className="flex items-center gap-2 text-[14px] font-bold"><KeyRound className="h-4 w-4 text-blue-600" />{keys.length} key</h2>
        <div className="mt-3 grid gap-2">
          {!loading && keys.length === 0 ? <p className="text-[12px] text-slate-400">Belum ada API key.</p> : null}
          {keys.map((k) => (
            <div key={k.id} className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-slate-100 p-3 dark:border-slate-800">
              <div className="min-w-0">
                <p className="truncate text-[13px] font-semibold">
                  {k.name} <span className="font-mono text-[11px] text-slate-500">{k.prefix}...</span>
                  {k.revoked ? <span className="ml-2 rounded bg-red-100 px-1.5 py-0.5 text-[10px] font-bold text-red-700 dark:bg-red-500/20 dark:text-red-300">REVOKED</span> : null}
                </p>
                <p className="text-[11px] text-slate-500">
                  dibuat {formatDate(k.createdAt)}{k.lastUsedAt ? <> · terakhir dipakai {formatDate(k.lastUsedAt)}</> : ' · belum pernah dipakai'}
                </p>
              </div>
              <div className="flex gap-2">
                {!k.revoked ? (
                  <Button size="sm" variant="danger" onClick={() => revoke(k.id)}><Trash2 className="h-4 w-4" />Revoke</Button>
                ) : (
                  <Button size="sm" variant="outline" onClick={() => hardDelete(k.id)}><Trash2 className="h-4 w-4" />Hapus</Button>
                )}
              </div>
            </div>
          ))}
        </div>
      </Card>
    </div>
  )
}
