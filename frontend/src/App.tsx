import { lazy, Suspense } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { ProtectedRoute } from '@/components/auth/ProtectedRoute'
import { DriveLayout } from '@/layouts/DriveLayout'
import { LoginPage } from '@/pages/LoginPage'
import { GoogleAuthPage } from '@/pages/GoogleAuthPage'
import GoogleConnectedPage from '@/pages/GoogleConnectedPage'
import { RegisterPage } from '@/pages/RegisterPage'
import { UploadProvider } from '@/context/UploadContext'

const AllFilesPage = lazy(() => import('@/pages/AllFilesPage').then(({ AllFilesPage }) => ({ default: AllFilesPage })))
const QuotaTrackerPage = lazy(() => import('@/pages/QuotaTrackerPage').then(({ QuotaTrackerPage }) => ({ default: QuotaTrackerPage })))
const SettingsPage = lazy(() => import('@/pages/SettingsPage').then(({ SettingsPage }) => ({ default: SettingsPage })))

function PageLoader() {
  return <main className="grid min-h-screen place-items-center text-sm font-semibold text-slate-500">Loading PanDrive…</main>
}

function App() {
  return (
    <UploadProvider>
      <Suspense fallback={<PageLoader />}>
        <Routes>
          <Route path="login" element={<LoginPage />} />
          <Route path="register" element={<RegisterPage />} />
          <Route path="google-auth" element={<GoogleAuthPage />} />
          <Route path="google-connected" element={<GoogleConnectedPage />} />
          <Route element={<ProtectedRoute />}>
            <Route element={<DriveLayout />}>
              <Route index element={<Navigate to="/all-files" replace />} />
              <Route path="all-files" element={<AllFilesPage />} />
              <Route path="quota" element={<QuotaTrackerPage />} />
              <Route path="settings" element={<SettingsPage />} />
            </Route>
          </Route>
          <Route path="*" element={<Navigate to="/all-files" replace />} />
        </Routes>
      </Suspense>
    </UploadProvider>
  )
}

export default App
