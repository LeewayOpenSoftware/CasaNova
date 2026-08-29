// Wallpaper catalog — the stock backgrounds CasaNova ships with.
//
// Files live under public/assets/wallpapers/ so they're served as
// static URLs (the theme store stores URLs, not import paths). The
// label is only used in the Appearance picker's hover tooltip; users
// pick visually from thumbnails.

export interface WallpaperEntry {
  id: string
  label: string
  url: string
}

export const wallpapersFixture: WallpaperEntry[] = [
  { id: 'mediterranean', label: 'Mediterranean living room', url: '/assets/wallpapers/wallpaper0a.png' },
  { id: 'sunlit', label: 'Sunlit interior', url: '/assets/wallpapers/wallpaper0b.png' },
  { id: 'courtyard', label: 'Courtyard garden', url: '/assets/wallpapers/wallpaper0c.png' },
  { id: 'terrace', label: 'Terrace afternoon', url: '/assets/wallpapers/wallpaper0d.png' },
  { id: 'garden', label: 'Garden overview', url: '/assets/wallpapers/wallpaper0e.png' },
  { id: 'village', label: 'Village path', url: '/assets/wallpapers/wallpaper0z.png' },
]
