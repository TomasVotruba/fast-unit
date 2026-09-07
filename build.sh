#!/usr/bin/env bash

# Cross-compiles the fastunit runner for every supported platform into bin/dist/.
# The resulting binaries are committed to the repo, so the PHP shim execs them
# directly with no network download. Re-run and commit the output on each release.

set -euo pipefail

cd "$(dirname "$0")"

out="bin/dist"
rm -rf "$out"
mkdir -p "$out"

platforms=(
    "linux amd64"
    "linux arm64"
    "darwin amd64"
    "darwin arm64"
    "windows amd64 .exe"
)

for platform in "${platforms[@]}"; do
    read -r goos goarch ext <<< "$platform"
    asset="fastunit-${goos}-${goarch}${ext:-}"
    echo "building ${asset}"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "${out}/${asset}" .
done

echo "done -> ${out}"
