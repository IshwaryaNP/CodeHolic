@echo off
set GOROOT=C:\Users\Ishwarya\go
set GOPATH=C:\Users\Ishwarya\go_path
set PATH=%GOROOT%\bin;%PATH%

echo Building kvdb.exe...
go build -o kvdb.exe ./cmd/kvdb
if %ERRORLEVEL% NEQ 0 (
    echo Failed to build kvdb.exe
    exit /b %ERRORLEVEL%
)

echo Building torture.exe...
go build -o torture.exe ./cmd/torture
if %ERRORLEVEL% NEQ 0 (
    echo Failed to build torture.exe
    exit /b %ERRORLEVEL%
)

echo Successfully built kvdb.exe and torture.exe

