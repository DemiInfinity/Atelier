# Atelier: Apple icon (Icon Composer)

1. Open Icon Composer (ships with Xcode 26+). New icon.
2. Background: set the solid colour #28625A.
3. Drag `2-foreground.svg` in as the foreground group. Split it into up to 4 groups if you want depth (back to front).
4. Do NOT add your own highlights, shadows, blur or glass: the system renders Liquid Glass.
5. Appearances: Default = these layers; Dark = darken the background, keep the same foreground; Mono = `mono-glyph.svg`.
6. Save `AppIcon.icon` into the Xcode project and select it as the app icon. Keep an asset catalogue/`AppIcon-1024.png` for older OS versions if needed.
`AppIcon-1024.png` is a flat, opaque fallback (no alpha, no rounded corners) for asset catalogues and stores.
