# Contributing

## Tests

```
make test    # go vet + go test -race ./... (rebuilds the C parts); needs no GPU and no ROM
make gl      # GPU path with a built-in triangle core (needs a GPU; one GPU test at a time)
```

Real-core tests are opt-in and use files on **your** disk. They are skipped when the variables are unset:

```
SNES=1 SNES_ROM=<your-snes-rom>  go test -run TestRealSNES -v .
N64=1  N64_ROM=<your-n64-rom>    go test -run 'TestRealN64$' -v .
PSP=1  PSP_ROM=<your-psp-image>  go test -run 'TestRealPSP$' -v .
```

They need the cores in `cores/` (see README). Run GPU tests one at a time.

## ROMs, cores, firmware

**Never** commit a ROM, ISO, core `.so`, BIOS, firmware or save. `bin/` and `cores/` are git-ignored. Do not put commercial
game names or personal paths in code, tests, scripts or docs: use environment variables and placeholders such as
`<your-rom>`. Test fixtures must be tiny and made for the purpose (see `testdata/save-zips`).

## Commits and style

- One theme per commit, imperative short subject, body explains why. No secrets, tokens or personal data in messages.
- Go: `gofmt`, `go vet` clean. C: keep `-Wall` clean. Comments explain why, not what.
- Keep the `TOTV_STREAM_*` variables and the socket/secret paths stable: they are the contract with the Totv.
- Security-relevant changes (sandbox, descriptors, limits) need a test that proves the new behavior fails closed.

By contributing you agree your work is licensed GPL-3.0-or-later.
