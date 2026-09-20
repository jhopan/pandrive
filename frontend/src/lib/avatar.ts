// Local, network-free avatars: a deterministic colour + initial rendered as an inline SVG data URL.
// No Gravatar/DiceBear/pravatar lookup, so avatars always render (offline, privacy blockers, CSP).
const PALETTE = ['#2563eb', '#7c3aed', '#db2777', '#ea580c', '#059669', '#0891b2', '#4f46e5', '#b45309']

export function avatarColor(seed: string): string {
  let hash = 0
  for (let i = 0; i < seed.length; i++) {
    hash = (hash << 5) - hash + seed.charCodeAt(i)
    hash |= 0
  }
  return PALETTE[Math.abs(hash) % PALETTE.length]
}

export function avatarInitial(seed: string | undefined): string {
  const value = (seed ?? '').trim()
  return (value.charAt(0) || 'U').toUpperCase()
}

export function getAvatarUrl(email: string | undefined, size = 96): string {
  const seed = (email ?? 'user').trim().toLowerCase()
  const initial = avatarInitial(seed)
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}" viewBox="0 0 96 96"><rect width="96" height="96" rx="48" fill="${avatarColor(seed)}"/><text x="48" y="48" fill="#ffffff" font-family="system-ui,-apple-system,Segoe UI,sans-serif" font-size="42" font-weight="700" text-anchor="middle" dominant-baseline="central">${initial}</text></svg>`
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}
