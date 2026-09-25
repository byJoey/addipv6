#!/usr/bin/env bash
# 交叉编译各平台的二进制，产物放 dist/。
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo v1.0.0)}"
OUT=dist
mkdir -p "$OUT"

targets=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
  "linux/386"
  "linux/riscv64"
)

echo "版本 $VERSION"
for t in "${targets[@]}"; do
  os="${t%/*}"
  arch="${t#*/}"
  name="addipv6-${os}-${arch}"
  env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM=7 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$OUT/$name" .
  echo "  $name  $(du -h "$OUT/$name" | cut -f1)"
done

( cd "$OUT" && shasum -a 256 addipv6-* > SHA256SUMS 2>/dev/null || sha256sum addipv6-* > SHA256SUMS )
echo "校验和写在 $OUT/SHA256SUMS"
