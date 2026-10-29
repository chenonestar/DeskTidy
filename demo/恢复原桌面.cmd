@echo off
chcp 65001 >nul
rem 不依赖 Demo 主界面：恢复系统桌面图标，并写回未恢复的图标位置
cd /d "%~dp0"
start "" /wait desktidy-demo.exe --restore
echo 已执行恢复，详情见 demo-data\demo-log.txt
pause
