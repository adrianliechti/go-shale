# Embedded uutils artifacts

Every executable is checked in, so ordinary Go consumers do not download or
build Rust. Each `<artifact>.sha256` records its SHA-256 and is checked by tests.
Command aliases share the artifact bytes without copying them.

| Artifact | Commands | Source | Archive SHA-256 | Size |
| --- | --- | --- | --- | --- |
| `coreutils.wasm` | 77 coreutils, `[`, `coreutils` | [uutils/coreutils 0.11.0](https://github.com/uutils/coreutils/tree/0.11.0) | `a47966117783bef18650cc724f1b1d061b717ac91a0feaabdd34910703cf70a4` | ~8 MB |
| `grep.wasm` | `grep` | [uutils/grep 0.2.0](https://github.com/uutils/grep/tree/0.2.0) | `aabb48c5f14aa46befa4b0b2848889d3ca2ff819d5855acfab590f7f47d223b9` | ~1.1 MB |
| `find.wasm` | `find` | [uutils/findutils 0.10.0](https://github.com/uutils/findutils/tree/0.10.0) | `e36ae3937f889bc59cfbd65820a642baa695c58d7fa1e387e41857e710f40419` | ~1.6 MB |
| `diffutils.wasm` | `diff`, `cmp` | [uutils/diffutils v0.5.0](https://github.com/uutils/diffutils/tree/v0.5.0) | `4c05d236ebddef7738446980a59cd13521b6990ea02242db6b32321dd93853ca` | ~1.1 MB |
| `sed.wasm` | `sed` | [uutils/sed 0.2.0](https://github.com/uutils/sed/tree/0.2.0) | `a5a03b3871b963420d1408b11415c90d4d69df02ab4f2f82826b42899a61a87a` | ~1.5 MB |

Source archives are `https://github.com/uutils/<project>/archive/refs/tags/<tag>.tar.gz`.

Common build settings:

- Rust: `1.98.1 (48a229cea 2026-09-01)`; target `wasm32-wasip1`; each project's
  upstream `Cargo.lock` (`--locked`)
- Release profile overrides: optimization `z`, strip `symbols`
- coreutils only: no default features, `feat_wasm`, binary `coreutils`
- C toolchain: [WASI SDK 33](https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-33)
  for the bundled Oniguruma in grep and findutils and for the constructor below
- These artifacts were built on macOS arm64 with `wasi-sdk-33.0-arm64-macos.tar.gz`.
  SDK archive SHA-256: `85c997a2665ead91673b5bb88b7d0df3fc8900df3bfa244f720d478187bbdc78`

`findutils` also ships `xargs`, `locate`, and `updatedb`; they are not embedded.
WASI cannot spawn processes, so the shell implements `xargs` itself and `find`'s
`-exec` family reports an error.

## Working directory

WASI libc starts every command at `/` and offers no way to seed its emulated
working directory from the host. The Go host sets `SHALE_CWD` to the shell's
current virtual directory on every invocation, and the guest changes to it
before `main` runs. Two mechanisms do this:

- [`coreutils-cwd.patch`](../../scripts/patches/coreutils-cwd.patch) changes only
  the multicall launcher of coreutils to call `set_current_dir`.
- [`shale_cwd.c`](../../scripts/wasi/shale_cwd.c) is a C constructor linked into
  every other artifact through `-C link-arg`. It needs no source patch, and can
  replace the coreutils patch at the next coreutils rebuild.

Merely setting `PWD` does not change the paths used by Rust file operations.
Relative-path and `cd` integration tests cover both mechanisms.

## Local patches

- [`diffutils-wasi.patch`](../../scripts/patches/diffutils-wasi.patch): `cmp`
  used Unix-only metadata extensions and stdout descriptor identity, which do
  not build for WASI; sizes now use the portable `len()` and the `/dev/null`
  detection returns false. `diff -u` headers format the UTC time fields
  directly, because chrono's local zone is unavailable on WASI and its `%M`
  formatter produces garbage on wasm32.
- [`sed-in-place.patch`](../../scripts/patches/sed-in-place.patch): upstream
  let `-i` consume a following argument as the backup suffix, so
  `sed -i 's/a/b/' file` treated the script as a suffix. The patch rewrites
  `-iSUFFIX` to `-i=SUFFIX` and requires the equals form, matching GNU, where
  only an attached suffix counts.

Utility implementations are otherwise unchanged. These are locally patched
builds, not unmodified upstream release binaries.

## Rebuild

Install Rust 1.98.1 and its `wasm32-wasip1` target, then extract WASI SDK 33 for
your build host. Run from the repository, optionally naming projects:

```sh
WASI_SDK_PATH=/absolute/path/to/wasi-sdk-33.0-arm64-macos sh scripts/build-wasm.sh
WASI_SDK_PATH=/absolute/path/to/wasi-sdk-33.0-arm64-macos sh scripts/build-wasm.sh grep sed
go test ./...
```

The script verifies each source archive, applies the checked-in patches, builds
with the locked dependencies, and replaces the artifacts, checksums, and license
copies. Build directories are retained under `.cache` for inspection. Toolchains,
host platforms, and paths can affect output bytes; bit-for-bit cross-host
reproducibility has not been established. When adding a project, register its
module and command names in `embed.go`.

## Licenses

All projects are MIT-licensed; diffutils is MIT OR Apache-2.0 and is used under
MIT. Copyright and license texts are retained in [LICENSE](LICENSE) (coreutils),
[LICENSE-grep](LICENSE-grep), [LICENSE-findutils](LICENSE-findutils),
[LICENSE-diffutils](LICENSE-diffutils), and [LICENSE-sed](LICENSE-sed). Rust
dependencies retain their own licenses; consult each pinned `Cargo.lock` and
dependency when preparing a redistributed release.
