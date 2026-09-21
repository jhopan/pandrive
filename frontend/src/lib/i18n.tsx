import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'

// Tiny ID/EN dictionary. Keys are English source strings; values are the Indonesian renders.
// A missing translation falls back to the key itself, so English needs zero entries.
const ID: Record<string, string> = {
  // Sidebar groups
  'Files': 'File',
  'Cleanup': 'Bersih-bersih',
  'System': 'Sistem',
  'Accounts': 'Akun',
  // Sidebar items
  'All Files': 'Semua File',
  'Starred': 'Berbintang',
  'Recent': 'Terbaru',
  'Search': 'Cari',
  'Gallery': 'Galeri',
  'Uploads': 'Unggahan',
  'Trash': 'Sampah',
  'Duplicates': 'Duplikat',
  'Activity': 'Aktivitas',
  'Shared': 'Dibagikan',
  'Health': 'Kesehatan',
  'Rate Limits': 'Batas Kuota',
  'Quota Tracker': 'Pelacak Kuota',
  'Settings': 'Pengaturan',
  'Log Out': 'Keluar',
  // Header / profile
  'Open profile menu': 'Buka menu profil',
  'Edit profile': 'Ubah profil',
  'Change password': 'Ganti sandi',
  'Log out': 'Keluar',
  'System info': 'Info sistem',
  'Toggle theme': 'Ganti tema',
  'Search Documents': 'Cari Dokumen',
  // Login
  'Login': 'Masuk',
  'Access your PanDrive gateway.': 'Akses gerbang PanDrive kamu.',
  'Email': 'Email',
  'Password': 'Kata Sandi',
  // Page titles
  'Upload File': 'Unggah File',
  'Storage': 'Penyimpanan',
  'People with access': 'Orang yang punya akses',
  'Public links': 'Tautan publik',
  'Refresh': 'Segarkan',
  'Cancel': 'Batal',
  'Save': 'Simpan',
  'Send test': 'Kirim tes',
  'Loading...': 'Memuat...',
  'Create folder': 'Buat folder',
  'New Folder': 'Folder Baru',
  'Upload': 'Unggah',
  'Sync': 'Sinkron',
  // Shared page bits
  'Expiring': 'Kedaluwarsa',
  'Expired': 'Kadaluarsa',
  'will be revoked automatically': 'akan dicabut otomatis',
  'Copy': 'Salin',
  'Copied': 'Tersalin',
  'Open': 'Buka',
  'Revoke': 'Cabut',
  'No public links.': 'Belum ada tautan publik.',
  'Share a file from All Files to create one.': 'Bagikan file dari Semua File untuk membuatnya.',
  'No per-person access granted. Right-click a file or folder and choose Invite Member.': 'Belum ada akses per-orang. Klik kanan file/folder lalu pilih Invite Member.',
}

type Lang = 'en' | 'id'

const LANG_KEY = 'pandrive.lang'
const LangContext = createContext<{ lang: Lang; setLang: (l: Lang) => void; t: (s: string) => string }>({
  lang: 'en',
  setLang: () => undefined,
  t: (s) => s,
})

function initialLang(): Lang {
  try {
    const stored = localStorage.getItem(LANG_KEY)
    if (stored === 'id' || stored === 'en') return stored
  } catch { /* private mode */ }
  return 'en'
}

// Wrap the app once (DriveLayout mount). t() reads the key verbatim in English mode.
export function I18nProvider({ children }: { children: ReactNode }) {
  const [lang, setLangState] = useState<Lang>(initialLang)
  const setLang = useCallback((next: Lang) => {
    setLangState(next)
    try { localStorage.setItem(LANG_KEY, next) } catch { /* private mode */ }
  }, [])
  useEffect(() => { document.documentElement.lang = lang }, [lang])
  const t = useCallback((s: string) => (lang === 'id' ? ID[s] ?? s : s), [lang])
  const value = useMemo(() => ({ lang, setLang, t }), [lang, setLang, t])
  return <LangContext.Provider value={value}>{children}</LangContext.Provider>
}

export function useI18n() {
  return useContext(LangContext)
}
