#!/bin/sh
# Rebuilds the checked-in WASM artifacts in internal/wasm from pinned uutils
# releases. Build dependencies are only needed to update those artifacts.
#
#   WASI_SDK_PATH=/path/to/wasi-sdk-33.0-<host> sh scripts/build-wasm.sh [project...]
#
# Projects: coreutils grep findutils diffutils sed (default: all).
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

: "${WASI_SDK_PATH:?Set WASI_SDK_PATH to an extracted WASI SDK 33 directory}"
case "$(rustc --version)" in
  "rustc 1.98.1 "*) ;;
  *) echo 'Use Rust 1.98.1 with the wasm32-wasip1 target installed.' >&2; exit 1 ;;
esac
if [ ! -x "$WASI_SDK_PATH/bin/clang" ]; then
  echo 'WASI_SDK_PATH must contain bin/clang and share/wasi-sysroot.' >&2
  exit 1
fi

mkdir -p "$repo_dir/.cache"
export CARGO_HOME="$repo_dir/.cache/cargo"
export CARGO_PROFILE_RELEASE_OPT_LEVEL=z
export CARGO_PROFILE_RELEASE_STRIP=symbols
# onig_sys (grep, findutils) compiles bundled C and needs the WASI sysroot.
export CC_wasm32_wasip1="$WASI_SDK_PATH/bin/clang"
export AR_wasm32_wasip1="$WASI_SDK_PATH/bin/llvm-ar"

# The constructor object seeds each command's working directory from SHALE_CWD.
# See scripts/wasi/shale_cwd.c. coreutils predates it and still uses a patch.
ctor="$repo_dir/.cache/shale_cwd.o"
"$WASI_SDK_PATH/bin/clang" --target=wasm32-wasip1 --sysroot="$WASI_SDK_PATH/share/wasi-sysroot" \
  -O2 -c "$repo_dir/scripts/wasi/shale_cwd.c" -o "$ctor"

build() {
  project=$1
  # tag: release tag; sha: SHA-256 of the GitHub source archive; patches: files in
  # scripts/patches; bin: cargo binary name; artifact: internal/wasm/<artifact>.wasm;
  # license: file in the source tree; features: extra cargo flags; cwd: patch or ctor.
  patches='' features='' cwd=ctor
  case $project in
    coreutils)
      tag=0.11.0 sha=a47966117783bef18650cc724f1b1d061b717ac91a0feaabdd34910703cf70a4
      patches=coreutils-cwd.patch bin=coreutils artifact=coreutils license=LICENSE dest=LICENSE
      features='--no-default-features --features feat_wasm' cwd=patch ;;
    grep)
      tag=0.2.0 sha=aabb48c5f14aa46befa4b0b2848889d3ca2ff819d5855acfab590f7f47d223b9
      bin=grep artifact=grep license=LICENSE dest=LICENSE-grep ;;
    findutils)
      tag=0.10.0 sha=e36ae3937f889bc59cfbd65820a642baa695c58d7fa1e387e41857e710f40419
      bin=find artifact=find license=LICENSE dest=LICENSE-findutils ;;
    diffutils)
      tag=v0.5.0 sha=4c05d236ebddef7738446980a59cd13521b6990ea02242db6b32321dd93853ca
      patches=diffutils-wasi.patch bin=diffutils artifact=diffutils license=LICENSE-MIT dest=LICENSE-diffutils ;;
    sed)
      tag=0.2.0 sha=a5a03b3871b963420d1408b11415c90d4d69df02ab4f2f82826b42899a61a87a
      patches=sed-in-place.patch bin=sed artifact=sed license=LICENSE dest=LICENSE-sed ;;
    *) echo "unknown project: $project" >&2; exit 1 ;;
  esac

  build_dir=$(mktemp -d "$repo_dir/.cache/$project-build.XXXXXX")
  echo "Building $project $tag in $build_dir (retained for inspection)"
  curl -fL --retry 3 "https://github.com/uutils/$project/archive/refs/tags/$tag.tar.gz" -o "$build_dir/source.tar.gz"
  actual_sha=$(shasum -a 256 "$build_dir/source.tar.gz" | cut -d ' ' -f 1)
  if [ "$actual_sha" != "$sha" ]; then
    echo "$project: source archive checksum mismatch." >&2
    exit 1
  fi
  mkdir "$build_dir/source"
  tar -xzf "$build_dir/source.tar.gz" -C "$build_dir/source" --strip-components=1
  for p in $patches; do
    patch -d "$build_dir/source" -p1 < "$repo_dir/scripts/patches/$p"
  done

  if [ "$cwd" = ctor ]; then
    export CARGO_TARGET_WASM32_WASIP1_RUSTFLAGS="-C link-arg=$ctor"
  else
    unset CARGO_TARGET_WASM32_WASIP1_RUSTFLAGS
  fi
  CARGO_TARGET_DIR="$build_dir/target" cargo build --manifest-path "$build_dir/source/Cargo.toml" \
    --locked --release --target wasm32-wasip1 $features --bin "$bin"

  cp "$build_dir/target/wasm32-wasip1/release/$bin.wasm" "$repo_dir/internal/wasm/$artifact.wasm"
  cp "$build_dir/source/$license" "$repo_dir/internal/wasm/$dest"
  (cd "$repo_dir/internal/wasm" && shasum -a 256 "$artifact.wasm" > "$artifact.sha256")
  echo "$project: updated internal/wasm/$artifact.wasm and $artifact.sha256"
}

if [ $# -eq 0 ]; then
  set -- coreutils grep findutils diffutils sed
fi
for project in "$@"; do
  build "$project"
done
echo 'Artifacts and checksums updated. Run go test ./... and review the diff.'
