# Licences

SeaTime is AGPL-3.0 (LICENSE). Everything below is compatible with it.

## Linked into seatime and seatimed

- Go standard library: BSD-3-Clause
- github.com/dustin/go-humanize v1.1.0: MIT
- github.com/google/uuid v1.6.0: BSD-3-Clause
- github.com/mattn/go-isatty v0.0.24: MIT
- github.com/ncruces/go-strftime v1.1.0: MIT
- github.com/remyoudompheng/bigfft (2023-01-29): BSD-3-Clause
- golang.org/x/sys v0.49.0: BSD-3-Clause
- modernc.org/libc v1.80.1: BSD-3-Clause (includes Go code, BSD-3-Clause; musl, MIT; go-netdb, MIT; nixpkgs, MIT)
- modernc.org/mathutil v1.7.1: BSD-3-Clause
- modernc.org/memory v1.12.1: BSD-3-Clause
- modernc.org/sqlite v1.62.0: BSD-3-Clause; SQLite itself is public domain

## Linked into seatime only

- fyne.io/systray v1.12.2 (the menu bar icon): Apache-2.0
- github.com/godbus/dbus/v5 v5.1.0 (the icon on Linux): BSD-2-Clause

## Linked into seatimed only

- github.com/jackc/pgx/v5 v5.13.0: MIT
- github.com/jackc/pgpassfile v1.0.0: MIT
- github.com/jackc/pgservicefile (2024-06-06): MIT
- github.com/jackc/puddle/v2 v2.2.3: MIT
- golang.org/x/sync v0.24.0: BSD-3-Clause
- golang.org/x/text v0.41.0: BSD-3-Clause

## Bundled assets

- M PLUS Rounded 1c (Latin subset, WOFF2) in the dashboard and server pages: SIL OFL 1.1, Copyright 2016 The Rounded M+ Project Authors (OFL.txt beside the fonts, embedded in both binaries)

## Shipped alongside, not linked

- The container base image gcr.io/distroless/static-debian12: Debian packages, each under its own licence

## Not in any binary

The other modules in the dependency graph (pprof, golang-lru [MPL-2.0], modernc tooling, x/tools, ...) are used only by tests, or are not compiled at all.

The installers use no third-party code.
