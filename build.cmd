@echo off
setlocal
cd /d "%~dp0"
set CGO_ENABLED=0
if not exist bin mkdir bin
if errorlevel 1 exit /b 1
go build -trimpath -ldflags "-s -w" -o bin\Lx-Cli.exe ./cmd/lx-cli
if errorlevel 1 exit /b 1
echo Built bin\Lx-Cli.exe
