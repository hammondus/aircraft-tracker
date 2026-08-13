# Vendored client libraries

Served from our own origin, never a CDN. An internal ops display must keep
working when a third party is down, and the pages must not leak who is looking
at them to anyone else.

Directories are pinned by version rather than content hash, because MapLibre's
ESM build imports `./maplibre-gl-shared.mjs` relatively -- a hashed filename
would break that import. The version in the path is the cache key: upgrading
means a new directory, so the URL changes with the bytes.

| Path | Version | Licence |
|---|---|---|
| `maplibre-gl@6.3.0/` | 6.3.0 | BSD-3-Clause |
| `pmtiles@4.5.0/` | 4.5.0 | BSD-3-Clause |
| `basemaps-assets@v4/` | v4 sprites + Noto Sans glyphs | Fonts SIL OFL 1.1; sprites BSD-3-Clause |

Glyphs cover ranges 0-255 and 256-511 only (Basic Latin, Latin-1, Latin
Extended-A), which is everything Australian place names need. Other ranges
would be dead weight in the binary.

`web/static/basemap-layers.json` is generated, not hand-written:

    npm install @protomaps/basemaps@5.7.2
    node -e "import('@protomaps/basemaps').then(({layers,namedFlavor}) => \
      console.log(JSON.stringify(layers('protomaps', namedFlavor('dark'), {lang:'en'}))))"

The style is assembled in app.js rather than stored whole, because the tile
source URL carries a cache-busting token only the server knows.
