import { useEffect, useMemo, useState } from 'react'
import { CopyX, RefreshCw, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes } from '@/lib/api'

type DupFile = { id: string; name: string; sizeBytes: string; accountEmail: string; folder: string; createdAt: string }
type DupGroup = { name: string; sizeBytes: string; count: number; wastedBytes: string; files: DupFile[] }

export function DuplicatesPage() {
  const [groups, setGroups] = useState<DupGroup[]>([])
  const [totalWasted, setTotalWasted] = useState('0')
  const [loading, setLoading] = useState(true)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')

  async function load() {
    setLoading(true)
    try {
      const data = await apiFetch<{ groups: DupGroup[]; totalWastedBytes: string }>('/files/duplicates')
      setGroups(data.groups)
      setTotalWasted(data.totalWastedBytes)
      setSelected(new Set())
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to scan duplicates')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load().catch(() => undefined)
  }, [])

  // Keeps the first (oldest copy) per group and marks the extra copies for cleanup.
  function keepFirstOfEachGroup() {
    const next = new Set<string>()
    for (const group of groups) {
      group.files.slice(1).forEach((f) => next.add(f.id))
    }
    setSelected(next)
  }

  const selectedBytes = useMemo(() => {
    let sum = 0
    for (const group of groups) {
      for (const f of group.files) {
        if (selected.has(f.id)) sum += Number(f.sizeBytes || 0)
      }
    }
    return sum
  }, [groups, selected])

  async function removeSelected() {
    if (selected.size === 0) return
    if (!confirm(`Delete ${selected.size} duplicate file(s)? They are removed from Google Drive permanently.`)) return
    setBusy(true)
    setMessage('')
    let ok = 0
    let failed = 0
    for (const id of selected) {
      try {
        await apiFetch(`/files/${id}/purge`, { method: 'POST' })
        ok++
      } catch {
        failed++
      }
    }
    setBusy(false)
    setMessage(`${ok} deleted${failed ? `, ${failed} failed` : ''}.`)
    await load()
    window.dispatchEvent(new Event('pandrive:storage-changed'))
  }

  return (
    <>
      <PageHeader
        title="Duplicates"
        description="Files with the same name and size, across every connected account."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Rescan</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-blue-50 p-3 text-sm text-blue-700">{message}</p> : null}

      <div className="mt-5 grid gap-4 sm:grid-cols-3">
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Duplicate groups</p><p className="mt-1 text-2xl font-extrabold">{groups.length}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Reclaimable</p><p className="mt-1 text-2xl font-extrabold text-amber-600">{formatBytes(totalWasted)}</p></Card>
        <Card className="p-4"><p className="text-xs font-bold uppercase text-slate-400">Selected</p><p className="mt-1 text-2xl font-extrabold text-blue-600">{selected.size} · {formatBytes(selectedBytes)}</p></Card>
      </div>

      <div className="mt-4 flex flex-wrap gap-2">
        <Button variant="outline" size="sm" onClick={keepFirstOfEachGroup} disabled={groups.length === 0}>Select extra copies</Button>
        <Button variant="outline" size="sm" onClick={() => setSelected(new Set())} disabled={selected.size === 0}>Clear selection</Button>
        <Button variant="danger" size="sm" onClick={removeSelected} disabled={busy || selected.size === 0}>
          <Trash2 className="h-4 w-4" />{busy ? 'Deleting...' : 'Delete selected'}
        </Button>
      </div>

      <div className="mt-5 grid gap-4">
        {loading ? (
          <Card className="p-4"><p className="text-sm text-slate-500">Scanning...</p></Card>
        ) : groups.length === 0 ? (
          <Card className="p-6 text-center">
            <CopyX className="mx-auto h-8 w-8 text-emerald-500" />
            <p className="mt-2 text-sm font-semibold">No duplicates found.</p>
            <p className="mt-1 text-[12px] text-slate-500">Every file has a unique name and size combination.</p>
          </Card>
        ) : (
          groups.map((group) => (
            <Card key={`${group.name}-${group.sizeBytes}`} className="p-4">
              <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-100 pb-2.5">
                <p className="truncate text-sm font-bold" title={group.name}>{group.name}</p>
                <p className="text-[12px] text-slate-500">{group.count} copies · {formatBytes(group.sizeBytes)} each · reclaim {formatBytes(group.wastedBytes)}</p>
              </div>
              <ul className="mt-2.5 grid gap-1.5">
                {group.files.map((file, index) => (
                  <li key={file.id} className="flex items-center gap-2.5 rounded-lg px-1 py-1.5 hover:bg-slate-50">
                    <input
                      type="checkbox"
                      checked={selected.has(file.id)}
                      onChange={(e) => {
                        setSelected((prev) => {
                          const next = new Set(prev)
                          if (e.target.checked) next.add(file.id)
                          else next.delete(file.id)
                          return next
                        })
                      }}
                    />
                    <span className="min-w-0 flex-1 truncate text-[13px]">{file.accountEmail}{file.folder ? ` · ${file.folder}` : ''}</span>
                    {index === 0 ? <span className="shrink-0 rounded-full bg-emerald-100 px-2 py-0.5 text-[10px] font-bold text-emerald-700">KEEP</span> : null}
                  </li>
                ))}
              </ul>
            </Card>
          ))
        )}
      </div>
    </>
  )
}
