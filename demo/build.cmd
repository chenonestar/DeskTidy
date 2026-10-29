@echo off
chcp 65001 >nul
rem 在 Windows 上编译 Demo（需要 Go 1.21 及以上，会自动下载 go.mod 要求的 1.26 工具链）
cd /d "%~dp0"
if not exist build mkdir build
go build -trimpath -tags desktop,production -ldflags "-H windowsgui -s -w" -o build\desktidy-demo.exe . || exit /b 1
copy /y "恢复原桌面.cmd" build\ >nul
echo 已生成 build\desktidy-demo.exe
