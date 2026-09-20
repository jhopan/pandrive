import { useEffect, useState } from 'react'
import { Download, FileText, FolderOpen, RefreshCw, Star, StarOff } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { API_URL, apiFetch, formatBytes, formatDate } from '@/lib/api'
import { getAccessToken } from '@/lib/auth'

type StarredFile = {
  id: string
  name: string
  mimeType: string
  sizeBytes: string
  createdAt: string
  updatedAt: string
  accountEmail: string
  folder: string
}
type StarredFolder = { id: string; name: string; color: string; iconUrl: string; updatedAt: string }

export function StarredPage() {
  const [files, setFiles] = useState<StarredFile[]>([])
  const [folders, setFolders] = useState<StarredFolder[]>([])
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [message, setMessage] = useState('')

  async function load() {
    setLoading(true)
    try {
      const data = await apiFetch<{ files: StarredFile[]; folders: StarredFolder[] }>('/starred')
      setFiles(data.files)
      setFolders(data.folders)
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load starred items')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load().catch(() => undefined)
  }, [])

  async function unstarFile(file: StarredFile) {
    setBusyId(file.id)
    try {
      await apiFetch(`/files/${file.id}/star`, { method: 'POST', body: JSON.stringify({ starred: false }) })
      setFiles((prev) => prev.filter((f) => f.id !== file.id))
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to unstar')
    } finally {
      setBusyId(null)
    }
  }

  async function unstarFolder(folder: StarredFolder) {
    setBusyId(folder.id)
    try {
      await apiFetch(`/folders/${folder.id}/star`, { method: 'POST', body: JSON.stringify({ starred: false }) })
      setFolders((prev) => prev.filter((f) => f.id !== folder.id))
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to unstar')
    } finally {
      setBusyId(null)
    }
  }

  async function download(file: StarredFile) {
    try {
      const response = await fetch(`${API_URL}/files/${file.id}/download`, {
        headers: { Authorization: `Bearer ${getAccessToken()}` },
      })
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

  const empty = files.length === 0 && folders.length === 0

  return (
    <>
      <PageHeader
        title="Starred"
        description="Files and folders you pinned for quick access."
        actions={<Button variant="outline" size="sm" onClick={load} disabled={loading}><RefreshCw className={loading ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />Refresh</Button>}
      />
      {message ? <p className="mt-4 rounded-xl bg-blue-50 p-3 text-sm text-blue-700">{message}</p> : null}

      {loading && empty ? (
        <Card className="mt-5 p-4"><p className="text-sm text-slate-500">Loading...</p></Card>
      ) : empty ? (
        <Card className="mt-5 p-6 text-center">
          <Star className="mx-auto h-8 w-8 text-slate-300" />
          <p className="mt-2 text-sm font-semibold">Nothing starred yet.</p>
          <p className="mt-1 text-[12px] text-slate-500">Right-click a file or folder and choose &quot;Add to Starred&quot;.</p>
        </Card>
      ) : (
        <div className="mt-5 grid gap-4 lg:grid-cols-2">
          <Card className="p-4">
            <div className="flex items-center justify-between border-b border-slate-100 pb-3">
              <h2 className="flex items-center gap-2 text-[16px] font-bold"><Star className="h-5 w-5 text-amber-500" />Files</h2>
              <span className="text-[12px] text-slate-500">{files.length}</span>
            </div>
            {files.length === 0 ? <p className="mt-3 text-sm text-slate-500">No starred files.</p> : (
              <ul className="mt-3 grid gap-2">
                {files.map((file) => (
                  <li key={file.id} className="flex flex-col gap-2 rounded-xl border border-slate-100 p-3 sm:flex-row sm:items-center sm:justify-between">
                    <div className="min-w-0">
                      <p className="flex items-center gap-2 truncate text-[13px] font-semibold" title={file.name}>
                        <FileText className="h-4 w-4 shrink-0 text-slate-400" />{file.name}
                      </p>
                      <p className="mt-0.5 text-[11px] text-slate-500">
                        {formatBytes(file.sizeBytes)}
                        {file.accountEmail ? <> · {file.accountEmail}</> : null}
                        {file.folder ? <> · {file.folder}</> : null}
                        {file.updatedAt ? <> · updated {formatDate(file.updatedAt)}</> : null}
                      </p>
                    </div>
                    <div className="flex shrink-0 gap-2">
                      <Button size="sm" variant="outline" onClick={() => download(file)}><Download className="h-4 w-4" />Download</Button>
                      <Button size="sm" variant="outline" onClick={() => unstarFile(file)} disabled={busyId === file.id}><StarOff className="h-4 w-4" />Unstar</Button>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card className="p-4">
            <div className="flex items-center justify-between border-b border-slate-100 pb-3">
              <h2 className="flex items-center gap-2 text-[16px] font-bold"><Star className="h-5 w-5 text-amber-500" />Folders</h2>
              <span className="text-[12px] text-slate-500">{folders.length}</span>
            </div>
            {folders.length === 0 ? <p className="mt-3 text-sm text-slate-500">No starred folders.</p> : (
              <ul className="mt-3 grid gap-2">
                {folders.map((folder) => (
                  <li key={folder.id} className="flex items-center gap-2 rounded-xl border border-slate-100 p-3">
                    <span className={`h-3 w-3 shrink-0 rounded-full ${folder.color || 'bg-blue-500'}`} style={folder.color ? { backgroundColor: folder.color } : undefined} />
                    <span className="min-w-0 flex-1 truncate text-[13px] font-semibold" title={folder.name}>{folder.name}</span>
                    <Button size="sm" variant="outline" onClick={() => { window.location.href = `/all-files?folderId=${folder.id}` }}>
                      <FolderOpen className="h-4 w-4" />Open
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => unstarFolder(folder)} disabled={busyId === folder.id}>
                      <StarOff className="h-4 w-4" />Unstar
                    </Button>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      )}
    </>
  )
}
