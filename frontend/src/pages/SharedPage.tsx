import { useEffect, useState } from 'react'
import { Copy, ExternalLink, Link2, Link2Off, RefreshCw, UserPlus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, formatDate } from '@/lib/api'

type Invite = {
  id: string
  targetType: string
  targetId: string
  email: string
  role: string
  createdAt: string
  targetName: string
  accountEmail: string
}
type Share = {
  id: string
  url: string
  createdAt: string
  fileId: string
  name: string
  sizeBytes: string
  mimeType: string
  accountEmail: string
}

export function SharedPage() {
  const [shares, setShares] = useState<Share[]>([])
  const [invites, setInvites] = useState<Invite[]>([])
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [message, setMessage] = useState('')
  const [copied, setCopied] = useState<string | null>(null)

  async function load() {
    setLoading(true)
    try {
      const [data, inviteData] = await Promise.all([
        apiFetch<{ shares: Share[] }>('/shares'),
        apiFetch<{ invites: Invite[] }>('/invites'),
      ])
      setShares(data.shares)
      setInvites(inviteData.invites)
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load shares')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load().catch(() => undefined)
  }, [])

  async function copy(share: Share) {
    try {
      await navigator.clipboard.writeText(share.url)
      setCopied(share.id)
      window.setTimeout(() => setCopied(null), 1500)
    } catch {
      setMessage('Clipboard blocked by the browser. Copy the link manually.')
    }
  }

  async function revoke(share: Share) {
    if (!confirm(`Revoke public access to "${share.name}"? The link stops working immediately.`)) return
    setBusyId(share.id)
    setMessage('')
    try {
      await apiFetch(`/shares/${share.id}`, { method: 'DELETE' })
      setShares((prev) => prev.filter((s) => s.id !== share.id))
      setMessage(`Public access revoked for "${share.name}".`)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Revoke failed')
    } finally {
      setBusyId(null)
    }
  }

  async function revokeInvite(invite: Invite) {
    if (!confirm(`Remove ${invite.email}'s access to "${invite.targetName}"?`)) return
    setBusyId(invite.id)
    setMessage('')
    try {
      await apiFetch(`/invites/${invite.id}`, { method: 'DELETE' })
      setInvites((prev) => prev.filter((i) => i.id !== invite.id))
      setMessage(`Access removed for ${invite.email}.`)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Revoke failed')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <>
      <PageHeader
        title="Shared"
        description="People you shared with, and files anyone with the link can read."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-blue-50 p-3 text-sm text-blue-700">{message}</p> : null}

      <Card className="mt-5 p-4">
        <div className="flex items-center justify-between border-b border-slate-100 pb-3">
          <h2 className="flex items-center gap-2 text-[16px] font-bold"><UserPlus className="h-5 w-5 text-emerald-500" />People with access</h2>
          <span className="text-[12px] text-slate-500">{invites.length} grant{invites.length === 1 ? '' : 's'}</span>
        </div>
        {invites.length === 0 ? (
          <p className="mt-3 text-sm text-slate-500">No per-person access granted. Right-click a file or folder and choose Invite Member.</p>
        ) : (
          <ul className="mt-3 grid gap-2">
            {invites.map((invite) => (
              <li key={invite.id} className="flex flex-col gap-2 rounded-xl border border-slate-100 p-3 sm:flex-row sm:items-center sm:justify-between">
                <div className="min-w-0">
                  <p className="truncate text-[13px] font-semibold">{invite.email} <span className="font-normal text-slate-500">· {invite.role}</span></p>
                  <p className="mt-0.5 text-[11px] text-slate-500">
                    {invite.targetType === 'folder' ? 'Folder' : 'File'}: {invite.targetName || invite.targetId}
                    {invite.accountEmail ? <> · via {invite.accountEmail}</> : null}
                    {invite.createdAt ? <> · {formatDate(invite.createdAt)}</> : null}
                  </p>
                </div>
                <Button size="sm" variant="danger" onClick={() => revokeInvite(invite)} disabled={busyId === invite.id}>
                  <Link2Off className="h-4 w-4" />Revoke
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Card>

      <Card className="mt-4 p-4">
        <div className="flex items-center justify-between border-b border-slate-100 pb-3">
          <h2 className="flex items-center gap-2 text-[16px] font-bold"><Link2 className="h-5 w-5 text-blue-500" />Public links</h2>
          <span className="text-[12px] text-slate-500">{shares.length} active</span>
        </div>

        {loading && shares.length === 0 ? (
          <p className="mt-4 text-sm text-slate-500">Loading...</p>
        ) : shares.length === 0 ? (
          <div className="py-6 text-center">
            <Link2Off className="mx-auto h-8 w-8 text-slate-300" />
            <p className="mt-2 text-sm font-semibold">No public links.</p>
            <p className="mt-1 text-[12px] text-slate-500">Share a file from All Files to create one.</p>
          </div>
        ) : (
          <ul className="mt-3 grid gap-2">
            {shares.map((share) => (
              <li key={share.id} className="flex flex-col gap-2 rounded-xl border border-slate-100 p-3 lg:flex-row lg:items-center lg:justify-between">
                <div className="min-w-0 flex-1">
                  <p className="truncate text-[13px] font-semibold" title={share.name}>{share.name}</p>
                  <p className="mt-0.5 text-[11px] text-slate-500">
                    {formatBytes(share.sizeBytes)} · {share.accountEmail || 'unknown account'}
                    {share.createdAt ? <> · shared {formatDate(share.createdAt)}</> : null}
                  </p>
                  <a href={share.url} target="_blank" rel="noreferrer" className="mt-1 block truncate text-[11px] text-blue-600 hover:underline" title={share.url}>{share.url}</a>
                </div>
                <div className="flex shrink-0 gap-2">
                  <Button size="sm" variant="outline" onClick={() => copy(share)}>
                    <Copy className="h-4 w-4" />{copied === share.id ? 'Copied' : 'Copy'}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => window.open(share.url, '_blank', 'noopener')}>
                    <ExternalLink className="h-4 w-4" />Open
                  </Button>
                  <Button size="sm" variant="danger" onClick={() => revoke(share)} disabled={busyId === share.id}>
                    <Link2Off className="h-4 w-4" />Revoke
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </>
  )
}
