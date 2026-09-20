import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'
import path from 'node:path'

export default defineConfig({
  plugins: [
    react(),
    VitePWA({
      registerType: 'autoUpdate',
      includeAssets: ['favicon.png', 'logo-192.png', 'logo.png', 'apple-touch-icon.png', 'maskable-icon.png'],
      manifest: {
        name: 'PanDrive',
        short_name: 'PanDrive',
        description: 'Google Drive storage gateway for files, folders, sharing, and quota tracking.',
        theme_color: '#2563eb',
        background_color: '#ffffff',
        display: 'standalone',
        start_url: '/all-files',
        scope: '/',
        orientation: 'portrait-primary',
        icons: [
          { src: '/logo-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/logo.png', sizes: '512x512', type: 'image/png' },
          { src: '/maskable-icon.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
        ],
      },
      workbox: {
        // `html` is deliberately NOT precached: the shell carries security headers (CSP/HSTS) and a
        // service-worker-cached copy would keep enforcing a stale policy long after the server changed
        // it (observed: folder icons stayed blocked after a CSP fix because the cached index.html
        // still had the old header). Serve the document from the network, precache only static assets.
        navigateFallback: null,
        navigateFallbackDenylist: [/^\/(auth|connected-accounts|files|folders|invites|provider-configs|public|storage|uploads|api)(\/|$)/],
        globPatterns: ['**/*.{js,css,svg,ico,png,webp,woff2}'],
        cleanupOutdatedCaches: true,
      },
    }),
  ],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
      'react': 'preact/compat',
      'react-dom/test-utils': 'preact/test-utils',
      'react-dom': 'preact/compat',
      'react/jsx-runtime': 'preact/jsx-runtime'
    },
  },
})
