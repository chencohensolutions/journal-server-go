# MSYS2's package signing keys

`journal-server setup` checks every MSYS2 package it downloads for libvips (`bundle/vips.go`) against these keys,
as pacman does. They are the keys of [MSYS2-keyring](https://github.com/msys2/MSYS2-keyring) (`msys2.gpg` at commit
`5d249077a0cfeec9dbf51515e5e01da413d28859`, Nov 19, 2026) without the ones it lists as revoked (`msys2-revoked`),
one file per key, named by its fingerprint, exported with `gpg --export --export-options export-minimal`.

When MSYS2 adds a packager key, setup refuses packages it signed until the key is added here: take `msys2.gpg` and
`msys2-revoked` from the keyring repository, and export each key that isn't revoked the same way.
