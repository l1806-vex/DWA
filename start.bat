@echo off
title DWA - DeepSeek Web API

cd /d "%~dp0"

echo.
echo  +------------------------------------------+
echo  ^|   DWA - DeepSeek Web API  ^|  v1.0.0      ^|
echo  +------------------------------------------+
echo  ^|  OpenAI :   http://127.0.0.1:8000/v1     ^|
echo  ^|  Anthropic: http://127.0.0.1:8000/v1     ^|
echo  +------------------------------------------+
echo.

where go >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Go not found. Install Go 1.22+ and add it to PATH.
    pause
    exit /b 1
)

if not exist bin\dwa.exe (
    echo [INFO] Building binary...
    go build -o bin\dwa.exe .
    if errorlevel 1 (
        echo [ERROR] Build failed.
        pause
        exit /b 1
    )
)

bin\dwa.exe %*
if errorlevel 1 (
    echo.
    echo [ERROR] Server exited with an error.
    pause
)
