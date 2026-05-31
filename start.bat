@echo off
title DWA — DeepSeek Web API

cd /d "%~dp0"

echo.
echo  +------------------------------------------+
echo  ^|   DWA — DeepSeek Web API  ^|  v1.0.0      ^|
echo  +------------------------------------------+
echo  ^|  OpenAI :  http://127.0.0.1:8000/v1      ^|
echo  ^|  Anthropic: http://127.0.0.1:8000/v1     ^|
echo  +------------------------------------------+
echo.

where go >nul 2>&1
if errorlevel 1 (
    echo [BLAD] Nie znaleziono Go. Zainstaluj Go 1.22+ i dodaj do PATH.
    pause
    exit /b 1
)

if not exist dwa.exe (
    echo [INFO] Buduje binarkę...
    go build -o dwa.exe .
    if errorlevel 1 (
        echo [BLAD] Build nie powiodł się.
        pause
        exit /b 1
    )
)

dwa.exe %*
if errorlevel 1 (
    echo.
    echo [BLAD] Serwer zakończył się błędem.
    pause
)
