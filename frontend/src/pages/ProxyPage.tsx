import { useCallback, useEffect, useState } from 'react'
import { Globe, RefreshCw, Save } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { Input } from '@/components/ui/input'
import { apiFetch } from '@/lib/api'

type ProxySettings = {
  provider: string
  domain: string
  caddy: { configPath: string; written: boolean; installed: boolean }
  tunnel: { enabled: boolean; mode: string }
}

// Reverse proxy manager: pick none / caddy / cloudflare, set the domain, and generate the
// Caddyfile (root+systemd reloads caddy automatically).
export function ProxyPage() {
  const [settings, setSettings] = useState<ProxySettings | null>(null)
  const [provider, setProvider] = useState('none')
  const [domain, setDomain] = useState('')
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const d = await apiFetch<ProxySettings>('/settings/proxy')
      setSettings(d)
      setProvider(d.provider || 'none')
      setDomain(d.domain || '')
      setMessage('')
    } catch (e) {
      setMessage(e instanceof Error ? e.message : 'Failed to load')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load().catch(() => undefined) }, [load])

  async function save() {
    setBusy(true); setMessage('')
    try {
      await apiFetch('/settings/proxy', { method: 'PUT', body: JSON.stringify({ provider, domain }) })
      setMessage('Tersimpan.')
      await load()
    } catch (e) { setMessage(e instanceof Error ? e.message : 'Gagal simpan') } finally { setBusy(false) }
  }

  async function writeCaddyfile() {
    setBusy(true); setMessage('')
    try {
      const r = await apiFetch<{ path: string; systemdReloaded: boolean }>('/settings/proxy/caddyfile', { method: 'POST', body: JSON.stringify({ domain }) })
      setMessage('Caddyfile ditulis: ' + r.path + (r.systemdReloaded ? ' — caddy direstart.' : ' — jalur manual: caddy start --config ' + r.path))
      await load()
    } catch (e) { setMessage(e instanceof Error ? e.message : 'Gagal menulis Caddyfile') } finally { setBusy(false) }
  }

  const Option = ({ id, title, desc }: { id: string; title: string; desc: string }) => (
    <label className={`flex cursor-pointer gap-3 rounded-xl border p-3 transition-colors ${provider === id ? 'border-blue-500 bg-blue-50/60 dark:bg-blue-500/10' : 'border-slate-200 hover:bg-slate-50 dark:border-slate-700 dark:hover:bg-slate-800/50'}`}>
      <input type="radio" name="provider" checked={provider === id} onChange={() => setProvider(id)} className="mt-1" />
      <span>
        <span className="block text-[13px] font-bold">{title}</span>
        <span className="block text-[11px] text-slate-500">{desc}</span>
      </span>
    </label>
  )

  return (
    <div className="mx-auto w-full max-w-3xl px-4 py-8 sm:px-6">
      <PageHeader
        title="Reverse Proxy"
        description="Atur cara PanDrive dijangkau dari internet: HTTPS langsung dengan Caddy, atau Cloudflare Tunnel."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={`h-4 w-4 ${loading ? 'animate-spin' : ''}`} />Refresh</Button>}
      />
      {message ? <Card className="mt-4 border-blue-200 bg-blue-50 p-3 text-[13px] font-semibold text-blue-700 dark:border-blue-500/30 dark:bg-blue-500/10 dark:text-blue-300">{message}</Card> : null}

      <Card className="mt-4 space-y-3 p-4">
        <h2 className="text-[14px] font-bold">Provider</h2>
        <Option id="none" title="Tanpa proxy (localhost saja)" desc="Akses via 127.0.0.1:4000 atau jaringan lokal." />
        <Option id="caddy" title="Caddy (HTTPS otomatis)" desc="Sertifikat & renew otomatis. Butuh DNS ke server ini + port 80/443." />
        <Option id="cloudflare" title="Cloudflare Tunnel" desc="Tanpa buka port; tunnel berjalan di dalam PanDrive (TUNNEL_ID/TUNNEL_TOKEN)." />

        <label className="grid gap-1 text-[12px] font-semibold">
          Domain
          <Input value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="drive.example.com" />
        </label>

        <div className="flex gap-2">
          <Button size="sm" disabled={busy} onClick={save}><Save className="h-4 w-4" />{busy ? 'Menyimpan...' : 'Save'}</Button>
        </div>
      </Card>

      <Card className="mt-4 space-y-2 p-4">
        <h2 className="flex items-center gap-2 text-[14px] font-bold"><Globe className="h-4 w-4 text-blue-600" />Caddy</h2>
        <p className="text-[12px] text-slate-500">
          Ter-install: <b>{settings?.caddy.installed ? 'ya' : 'belum'}</b> · Caddyfile: <b>{settings?.caddy.written ? settings.caddy.configPath : 'belum ditulis'}</b>
        </p>
        <Button size="sm" variant="outline" disabled={busy || !domain} onClick={writeCaddyfile}>Tulis Caddyfile untuk domain ini</Button>
        <p className="text-[11px] text-slate-400">Ditulis ke config dir; saat root+systemd juga ke /etc/caddy dan caddy direstart otomatis. DNS harus mengarah ke server, port 80/443 terbuka.</p>
      </Card>

      <Card className="mt-4 space-y-2 p-4">
        <h2 className="text-[14px] font-bold">Cloudflare Tunnel</h2>
        <p className="text-[12px] text-slate-500">
          Status: <b>{settings?.tunnel.enabled ? 'aktif (' + settings.tunnel.mode + ')' : 'tidak aktif'}</b>. Set TUNNEL_ID + tunnel.yml (atau TUNNEL_TOKEN) di .env lalu restart.
        </p>
      </Card>
    </div>
  )
}
