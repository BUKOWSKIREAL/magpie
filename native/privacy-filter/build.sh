#!/usr/bin/env bash
# build.sh <darwin-arm64|darwin-amd64> <out.zip>: macOS pf-serve.
# privacy-filter.cpp and ggml use the pinned commits and patches below.
# Apple Silicon uses Metal; Intel uses the CPU backend.
set -euo pipefail

PF_REPO=https://github.com/localai-org/privacy-filter.cpp
PF_COMMIT=735a6c28607ee82afc3a670383f41b55266a3b9a
target=$1
case $target in
  darwin-arm64|darwin-amd64) ;;
  *) echo "unsupported target $target: this fork builds macOS only" >&2; exit 2 ;;
esac
out=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
here=$(cd "$(dirname "$0")" && pwd)
work=${PF_WORK:-$here/.work}
src=$work/privacy-filter.cpp
mkdir -p "$work"

if [ ! -d "$src/.git" ]; then
  git init -q "$src"
  git -C "$src" remote add origin "$PF_REPO"
fi
if [ "$(git -C "$src" rev-parse -q --verify HEAD || true)" != "$PF_COMMIT" ]; then
  git -C "$src" fetch -q --depth 1 origin "$PF_COMMIT"
  git -C "$src" checkout -q --force FETCH_HEAD
fi
git -C "$src" submodule update -q --init --depth 1 ggml
git -C "$src" checkout -q -- .
git -C "$src/ggml" checkout -q -- .
for p in "$here"/patches/*.patch; do
  git -C "$src" apply "$p"
done

jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
stage=$work/stage-$target
rm -rf "$stage" && mkdir -p "$stage"

# build <name> <cmake args…>: pf-serve built with those, copied to the
# stage as name
build() {
  local name=$1
  shift
  local b=$work/build-$target-$name
  cmake -S "$here" -B "$b" -G Ninja -DCMAKE_BUILD_TYPE=Release -DPF_SRC="$src" \
    -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF "$@"
  cmake --build "$b" -j "$jobs" --target pf-serve
  cp "$b/pf-serve" "$stage/$name"
}

x86=(-DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DGGML_BMI2=ON)
case $target in
  darwin-arm64)
    build pf-serve -DCMAKE_OSX_ARCHITECTURES=arm64 -DCMAKE_OSX_DEPLOYMENT_TARGET=12.0 \
      -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON -DGGML_BLAS=OFF
    ;;
  darwin-amd64)
    build pf-serve -DCMAKE_SYSTEM_NAME=Darwin -DCMAKE_SYSTEM_PROCESSOR=x86_64 -DCMAKE_OSX_ARCHITECTURES=x86_64 \
      -DCMAKE_OSX_DEPLOYMENT_TARGET=12.0 -DGGML_METAL=OFF -DGGML_BLAS=OFF "${x86[@]}"
    ;;

esac

cp "$src/LICENSE" "$stage/LICENSE-privacy-filter.cpp"
cp "$src/ggml/LICENSE" "$stage/LICENSE-ggml"
rm -f "$out"
(cd "$stage" && zip -q -X -r "$out" .)
ls -l "$out"
