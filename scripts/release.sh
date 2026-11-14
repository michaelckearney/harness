#!/bin/sh
# Releases the harness from main: tags the next version, from the pull
# requests merged since the last tag, and publishes the binaries: the
# monorepos' pipelines download them, and harness deploy installs them on
# the servers.
set -eu
tag="$(go run ./scripts/nextversion)"
[ -n "$tag" ] || { echo "nothing to release"; exit 0; }
out="$(mktemp -d)"
for os in linux darwin; do
	for arch in amd64 arm64; do
		GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
			-ldflags "-X github.com/michaelckearney/harness/internal/version.set=$tag" \
			-o "$out/harness-$os-$arch" ./cmd/harness
	done
done
(cd "$out" && sha256sum harness-* > SHA256SUMS)
gh release create "$tag" --target "$(git rev-parse HEAD)" --title "$tag" --generate-notes "$out"/*
