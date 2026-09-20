import { useCallback, useEffect, useMemo, useState } from 'react'
import { BookmarkPlus, Download, FileText, FolderOpen, RefreshCw, Search, Star, Trash2, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { PageHeader } from '@/components/drive/PageHeader'
import { API_URL, apiFetch, formatBytes, formatDate } from '@/lib/api'
import { getAccessToken } from '@/lib/auth'

type SearchFile = {
  id: string
  name: string
  mimeType: string
  sizeBytes: string
  createdAt: string
  updatedAt: string
  starred: boolean
  folder: { id: string; name: string } | null
  connectedAccount: { id: string; email: string }
}
type Facet = { key: string; label: string; count: number }
type Results = {
  files: SearchFile[]
  total: number
  totalBytes: string
  limit: number
  offset: number
  facets: { accounts: Facet[]; kinds: Facet[] }
}
type Account = { id: string; email: string }
type SavedSearch = { name: string; query: string }

const KIND_LABELS: Record<string, string> = {
  image: 'Images', video: 'Video', audio: 'Audio', pdf: 'PDF', doc: 'Documents', archive: 'Archives', gapps: 'Google Docs', other: 'Other',
}
const SAVED_KEY = 'pandrive.savedSearches'

export function SearchPage() {
  const [query, setQuery] = useState('')
  const [kind, setKind] = useState('')
  const [accountId, setAccountId] = useState('')
  const [minSize, setMinSize] = useState('')
  const [maxSize, setMaxSize] = useState('')
  const [startDate, setStartDate] = useState('')
  const [endDate, setEndDate] = useState('')
  const [starredOnly, setStarredOnly] = useState(false)
  const [sort, setSort] = useState('date')
  const [limit, setLimit] = useState(50)
  const [results, setResults] = useState<Results | null>(null)
  const [accounts, setAccounts] = useState<Account[]>([])
  const [loading, setLoading] = useState(false)
  const [message, setMessage] = useState('')
  const [busyId, setBusyId] = useState<string | null>(null)
  const [saved, setSaved] = useState<SavedSearch[]>(() => {
    try {
      return JSON.parse(window.localStorage.getItem(SAVED_KEY) ?? '[]') as SavedSearch[]
    } catch {
      return []
    }
  })

  const params = useMemo(() => {
    const search = new URLSearchParams({ limit: String(limit), sort })
    if (query.trim()) search.set('q', query.trim())
    if (kind) search.set('kind', kind)
    if (accountId) search.set('accountId', accountId)
    if (minSize) search.set('minSize', minSize)
    if (maxSize) search.set('maxSize', maxSize)
    if (startDate) search.set('startDate', startDate)
    if (endDate) search.set('endDate', endDate)
    if (starredOnly) search.set('starred', '1')
    return search.toString()
  }, [query, kind, accountId, minSize, maxSize, startDate, endDate, starredOnly, sort, limit])

  const run = useCallback(async (queryString: string) => {
    setLoading(true)
    try {
      setResults(await apiFetch<Results>(`/search?${queryString}`))
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Search failed')
    } finally {
      setLoading(false)
    }
  }, [])

  // Debounce so typing does not fire a request per keystroke.
  useEffect(() => {
    const timer = window.setTimeout(() => {
      run(params).catch(() => undefined)
    }, 300)
    return () => window.clearTimeout(timer)
  }, [params, run])

  useEffect(() => {
    apiFetch<{ accounts: Account[] }>('/connected-accounts').then((d) => setAccounts(d.accounts)).catch(() => undefined)
  }, [])

  function persistSaved(next: SavedSearch[]) {
    setSaved(next)
    window.localStorage.setItem(SAVED_KEY, JSON.stringify(next))
  }

  function saveCurrent() {
    const name = window.prompt('Name this search', query.trim() || 'Filtered search')
    if (!name) return
    persistSaved([...saved.filter((s) => s.name !== name), { name, query: params.replace(/^limit=[^&]*&?/, '').replace(/^sort=[^&]*&?/, '') }])
  }

  function clearAll() {
    setQuery('')
    setKind('')
    setAccountId('')
    setMinSize('')
    setMaxSize('')
    setStartDate('')
    setEndDate('')
    setStarredOnly(false)
  }

  async function download(file: SearchFile) {
    try {
      const response = await fetch(`${API_URL}/files/${file.id}/download`, { headers: { Authorization: `Bearer ${getAccessToken()}` } })
      if (!response.ok) throw new Error('Download failed')
      const blob = await response.blob()
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = file.name
      anchor.click()
      URL.revokeObjectURL(url)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Download failed')
    }
  }

  async function toggleStar(file: SearchFile) {
    setBusyId(file.id)
    const next = !file.starred
    setResults((prev) => prev ? { ...prev, files: prev.files.map((f) => (f.id === file.id ? { ...f, starred: next } : f)) } : prev)
    try {
      await apiFetch(`/files/${file.id}/star`, { method: 'POST', body: JSON.stringify({ starred: next }) })
    } catch (error) {
      setResults((prev) => prev ? { ...prev, files: prev.files.map((f) => (f.id === file.id ? { ...f, starred: !next } : f)) } : prev)
      setMessage(error instanceof Error ? error.message : 'Failed to update starred')
    } finally {
      setBusyId(null)
    }
  }

  const activeFilters = [kind && 'type', accountId && 'account', (minSize || maxSize) && 'size', (startDate || endDate) && 'date', starredOnly && 'starred'].filter(Boolean) as string[]

  return (
    <>
      <PageHeader
        title="Search"
        description="Filter every indexed file by name, type, account, size, date and starred state."
        actions={
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={saveCurrent} disabled={!query.trim() && activeFilters.length === 0}>
              <BookmarkPlus className="h-4 w-4" />Save
            </Button>
            <Button variant="outline" size="sm" onClick={() => run(params)} disabled={loading}>
              <RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Run
            </Button>
          </div>
        }
      />
      {message ? <p className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">{message}</p> : null}

      <Card className="mt-5 p-4">
        <div className="flex items-center gap-2.5">
          <Search className="h-5 w-5 shrink-0 text-slate-400" />
          <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search by file name..." className="h-11" />
          {query || activeFilters.length ? <Button variant="ghost" size="sm" onClick={clearAll}><X className="h-4 w-4" />Clear</Button> : null}
        </div>

        <div className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Type
            <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="">Any type</option>
              {Object.entries(KIND_LABELS).map(([k, label]) => <option key={k} value={k}>{label}</option>)}
            </select>
          </label>
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Account
            <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={accountId} onChange={(e) => setAccountId(e.target.value)}>
              <option value="">All accounts</option>
              {accounts.map((a) => <option key={a.id} value={a.id}>{a.email}</option>)}
            </select>
          </label>
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Sort
            <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={sort} onChange={(e) => setSort(e.target.value)}>
              <option value="date">Last changed</option>
              <option value="created">Newest added</option>
              <option value="oldest">Oldest added</option>
              <option value="size">Largest first</option>
              <option value="size_asc">Smallest first</option>
              <option value="name">Name A-Z</option>
              <option value="name_desc">Name Z-A</option>
            </select>
          </label>
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Size range (bytes)
            <div className="flex items-center gap-2">
              <Input value={minSize} onChange={(e) => setMinSize(e.target.value.replace(/\D/g, ''))} placeholder="min" className="h-10" />
              <span className="text-slate-400">-</span>
              <Input value={maxSize} onChange={(e) => setMaxSize(e.target.value.replace(/\D/g, ''))} placeholder="max" className="h-10" />
            </div>
          </label>
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Added from
            <Input type="date" value={startDate} onChange={(e) => setStartDate(e.target.value)} className="h-10" />
          </label>
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Added to
            <Input type="date" value={endDate} onChange={(e) => setEndDate(e.target.value)} className="h-10" />
          </label>
          <label className="flex items-end gap-2 pb-2.5 text-[13px] font-semibold text-slate-600">
            <input type="checkbox" checked={starredOnly} onChange={(e) => setStarredOnly(e.target.checked)} />
            Starred only
          </label>
          <label className="grid gap-1.5 text-xs font-bold text-slate-500">
            Show
            <select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm font-normal" value={String(limit)} onChange={(e) => setLimit(Number(e.target.value))}>
              {[25, 50, 100, 250, 500].map((n) => <option key={n} value={n}>{n} results</option>)}
            </select>
          </label>
        </div>

        {saved.length > 0 ? (
          <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-slate-100 pt-3">
            <span className="text-[11px] font-bold uppercase text-slate-400">Saved</span>
            {saved.map((s) => (
              <span key={s.name} className="flex items-center gap-1 rounded-full bg-slate-100 py-1 pl-3 pr-1 text-[12px] font-semibold text-slate-600">
                <button type="button" onClick={() => run(`limit=${limit}&sort=${sort}&${s.query}`)}>{s.name}</button>
                <button type="button" aria-label={`Remove ${s.name}`} onClick={() => persistSaved(saved.filter((x) => x.name !== s.name))} className="rounded-full p-1 hover:bg-slate-200">
                  <Trash2 className="h-3 w-3" />
                </button>
              </span>
            ))}
          </div>
        ) : null}
      </Card>

      {results ? (
        <>
          <div className="mt-4 flex flex-wrap items-center gap-3">
            <span className="text-[13px] font-semibold">{results.total} result{results.total === 1 ? '' : 's'} · {formatBytes(results.totalBytes)}</span>
            {results.facets.accounts.length > 1 ? results.facets.accounts.map((facet) => (
              <button key={facet.key} type="button" onClick={() => setAccountId(facet.key === accountId ? '' : facet.key)}
                className={`rounded-full px-2.5 py-1 text-[11px] font-bold ${facet.key === accountId ? 'bg-blue-600 text-white' : 'bg-slate-100 text-slate-600'}`}>
                {facet.label} {facet.count}
              </button>
            )) : null}
            {results.facets.kinds.map((facet) => (
              <button key={facet.key} type="button" onClick={() => setKind(facet.key === kind ? '' : facet.key)}
                className={`rounded-full px-2.5 py-1 text-[11px] font-bold ${facet.key === kind ? 'bg-blue-600 text-white' : 'bg-slate-100 text-slate-600'}`}>
                {KIND_LABELS[facet.key] ?? facet.key} {facet.count}
              </button>
            ))}
          </div>

          <Card className="mt-3 p-4">
            {loading && results.files.length === 0 ? <p className="text-sm text-slate-500">Searching...</p> : results.files.length === 0 ? (
              <div className="py-6 text-center">
                <Search className="mx-auto h-8 w-8 text-slate-300" />
                <p className="mt-2 text-sm font-semibold">Nothing matches those filters.</p>
              </div>
            ) : (
              <ul className="grid gap-1">
                {results.files.map((file) => (
                  <li key={file.id} className="flex flex-col gap-2 rounded-xl px-2 py-2 hover:bg-slate-50 sm:flex-row sm:items-center sm:justify-between">
                    <div className="flex min-w-0 flex-1 items-start gap-2.5">
                      <FileText className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" />
                      <div className="min-w-0">
                        <p className="flex items-center gap-2 truncate text-[13px] font-semibold" title={file.name}>
                          {file.name}
                          {file.starred ? <Star className="h-3.5 w-3.5 shrink-0 fill-amber-400 text-amber-400" /> : null}
                        </p>
                        <p className="mt-0.5 text-[11px] text-slate-500">
                          {formatBytes(file.sizeBytes)} · {file.connectedAccount?.email}
                          {file.folder?.name ? <> · {file.folder.name}</> : null}
                          {file.createdAt ? <> · added {formatDate(file.createdAt)}</> : null}
                        </p>
                      </div>
                    </div>
                    <div className="flex shrink-0 gap-2">
                      {file.folder?.id ? (
                        <Button size="sm" variant="outline" onClick={() => { window.location.href = `/all-files?folderId=${file.folder?.id}` }}>
                          <FolderOpen className="h-4 w-4" />Folder
                        </Button>
                      ) : null}
                      <Button size="sm" variant="outline" onClick={() => toggleStar(file)} disabled={busyId === file.id}>
                        <Star className={file.starred ? 'h-4 w-4 fill-amber-400 text-amber-400' : 'h-4 w-4'} />
                      </Button>
                      <Button size="sm" variant="outline" onClick={() => download(file)}><Download className="h-4 w-4" />Download</Button>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </>
      ) : null}
    </>
  )
}
