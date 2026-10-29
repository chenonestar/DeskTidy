#!/usr/bin/env bash
# 在 Linux/macOS 上交叉编译 Windows 版 Demo，输出到 build/
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p build
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags desktop,production \
  -ldflags "-H windowsgui -s -w" -o build/desktidy-demo.exe .
cp 恢复原桌面.cmd build/
echo "已生成 build/desktidy-demo.exe"
