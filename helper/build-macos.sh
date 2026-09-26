#!/bin/sh
# Builds the universal macOS helper that ships inside plugin/bin.
#
# The helper is compiled for Apple Silicon and Intel from a single source
# revision and combined with lipo, so one packaged binary serves every Mac.
# The result is ad-hoc signed because macOS refuses to execute unsigned arm64
# binaries.
set -eu

# Pin the toolchain declared in helper/go.mod so committed artifacts stay
# reproducible. Go 1.21+ downloads and reuses that exact toolchain whenever the
# local installation differs.
export GOTOOLCHAIN=go1.25.4
toolchain_version=$(go version | awk '{print $3}')
if [ "$toolchain_version" != "go1.25.4" ]; then
  echo "Go 1.25.4 is required; found: ${toolchain_version#go}" >&2
  exit 1
fi

if [ "$(uname -s)" != "Darwin" ]; then
  echo "A macOS host with lipo and codesign is required to build the universal helper" >&2
  exit 1
fi

helper_directory=$(cd "$(dirname "$0")" && pwd)
repository_root=$(cd "${helper_directory}/.." && pwd)
output_directory="${repository_root}/plugin/bin"
output="${output_directory}/echo-music-keeper-helper-macos"

version=$(node -p "require('${repository_root}/package.json').version")
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *)
    echo "package.json contains an invalid version: $version" >&2
    exit 1
    ;;
esac

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

cd "$helper_directory"
for architecture in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$architecture" go build \
    -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=${version}" \
    -o "${staging}/echo-music-keeper-helper-${architecture}" \
    ./cmd/echo-music-keeper-helper
  if [ ! -x "${staging}/echo-music-keeper-helper-${architecture}" ]; then
    echo "echo-music-keeper-helper-${architecture} was not created" >&2
    exit 1
  fi
done

mkdir -p "$output_directory"
lipo -create -output "$output" \
  "${staging}/echo-music-keeper-helper-arm64" \
  "${staging}/echo-music-keeper-helper-amd64"
codesign --force --sign - "$output"
codesign --verify --strict "$output"
chmod 755 "$output"

echo "built $(file -b "$output")"
lipo -archs "$output"
shasum -a 256 "$output"
