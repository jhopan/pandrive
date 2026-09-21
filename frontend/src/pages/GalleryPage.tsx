import { useCallback, useEffect, useState } from 'react'
import { Image as ImageIcon, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, formatDate } from '@/lib/api'

type GalleryItem = {
  id: string
  name: string
  mimeType: string
  sizeBytes: string
  thumbnailUrl: string
  updatedAt: string
  connectedAccount: { id: string; email: string }
  folder?: { id: string; name: string }
}

// Thumbnails come from Google's CDN (thumbnailLink), so the browser talks to Google directly:
// PanDrive's bandwidth stays near zero. onError falls back to an icon tile.
export function GalleryPage() {
  const [items, setItems] = useState<GalleryItem[]>([])
  const [loading, setLoading] = useState(true)
  const [message, setMessage] = useState('')
  const [failed, setFailed] = useState<Record<string, boolean>>({})

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await apiFetch<{ items: GalleryItem[]; total: number }>('/gallery')
      setItems(data.items)
      setMessage('')
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to load gallery')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load().catch(() => undefined)
  }, [load])

  const isVideo = (mime: string) => mime.startsWith('video/')

  return (
    <div className="mx-auto w-full max-w-6xl px-4 py-8 sm:px-6">
      <PageHeader
        title="Gallery"
        description="Image dan video dari semua akun Drive. Thumbnail dimuat langsung dari Google (hemat bandwidth)."
        actions={
          <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
            <RefreshCw className={`h-4 w-4 ${loading ? 'animate-spin' : ''}`} />
            Refresh
          </Button>
        }
      />

      {message ? (
        <Card className="mb-4 border-red-200 bg-red-50 p-4 text-sm font-semibold text-red-700">{message}</Card>
      ) : null}

      {!loading && items.length === 0 ? (
        <Card className="grid place-items-center gap-2 p-12 text-center">
          <ImageIcon className="h-10 w-10 text-slate-300" />
          <p className="text-sm font-semibold text-slate-500">Belum ada image/video.</p>
          <p className="text-xs text-slate-400">Sync akun Drive dulu, atau upload media.</p>
        </Card>
      ) : null}

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5">
        {items.map((item) => (
          <Card key={item.id} className="group overflow-hidden p-0">
            <div className="relative aspect-square bg-slate-100 dark:bg-slate-800">
              {failed[item.id] || !item.thumbnailUrl ? (
                <div className="grid h-full place-items-center text-slate-300">
                  {isVideo(item.mimeType) ? <span className="text-3xl">🎬</span> : <ImageIcon className="h-10 w-10" />}
                </div>
              ) : (
                <img
                  src={item.thumbnailUrl}
                  alt={item.name}
                  loading="lazy"
                  referrerPolicy="no-referrer"
                  onError={() => setFailed((prev) => ({ ...prev, [item.id]: true }))}
                  className="h-full w-full object-cover transition-transform duration-300 group-hover:scale-105"
                />
              )}
              {isVideo(item.mimeType) ? (
                <span className="absolute left-2 top-2 rounded-md bg-slate-900/80 px-1.5 py-0.5 text-[10px] font-bold text-white">VIDEO</span>
              ) : null}
            </div>
            <div className="space-y-0.5 p-3">
              <p className="truncate text-[13px] font-bold" title={item.name}>{item.name}</p>
              <p className="text-[11px] text-slate-500">{formatBytes(Number(item.sizeBytes))} · {formatDate(item.updatedAt)}</p>
              <p className="truncate text-[11px] text-slate-400" title={item.connectedAccount.email}>{item.connectedAccount.email}{item.folder ? ` · ${item.folder.name}` : ''}</p>
            </div>
          </Card>
        ))}
      </div>
    </div>
  )
}
