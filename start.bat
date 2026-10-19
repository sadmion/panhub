@echo off
chcp 65001 >nul
cd /d %~dp0
echo ============================================
echo   panhub   http://127.0.0.1:5211
echo   默认密码 admin（请改 config.yaml）
echo   关闭这个窗口或按 Ctrl+C 即停止
echo ============================================
echo.
panhub.exe -config config.yaml
if errorlevel 1 pause
