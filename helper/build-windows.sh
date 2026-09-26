#!/bin/sh
# Builds the Windows x64 helper that ships inside plugin/bin.
#
# This is the POSIX counterpart of build.ps1 for macOS and Linux hosts. The
# result is byte-for-byte reproducible for one Go release, so the committed
# plugin/bin artifact can be verified by rebuilding it.
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

helper_directory=$(cd "$(dirname "$0")" && pwd)
repository_root=$(cd "${helper_directory}/.." && pwd)
output_directory="${repository_root}/plugin/bin"
output="${output_directory}/echo-music-keeper-helper.exe"

version=$(node -p "require('${repository_root}/package.json').version")
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *)
    echo "package.json contains an invalid version: $version" >&2
    exit 1
    ;;
esac

mkdir -p "$output_directory"
cd "$helper_directory"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
  -buildvcs=false -trimpath \
  -ldflags "-s -w -X main.version=${version}" \
  -o "$output" \
  ./cmd/echo-music-keeper-helper

if [ ! -f "$output" ]; then
  echo "echo-music-keeper-helper.exe was not created" >&2
  exit 1
fi

# Keep the committed artifact mode stable: git records the executable bit, and
# go build creates a 0755 file on POSIX hosts.
chmod 644 "$output"

echo "built ${output}"
shasum -a 256 "$output" 2>/dev/null || sha256sum "$output"
