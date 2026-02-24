@echo off
setlocal EnableExtensions
setlocal EnableDelayedExpansion

rem Usage:
rem   batch-run.bat "D:\dir1" "D:\dir2" "D:\dir with spaces"
rem
rem Optional: override defaults via env vars
rem   set QBIT_EXE=D:\tools\qbit-upload.exe
rem   set QBIT_COMMON_ARGS=--config "D:\tools\qbit-upload.yaml"
rem   batch-run.bat "D:\dir1" "D:\dir2"

if "%~1"=="" (
  echo Usage: %~nx0 "dir1" "dir2" ...
  exit /b 1
)

if not defined QBIT_EXE (
  set "QBIT_EXE=%~dp0qbit-upload.exe"
)

if not exist "%QBIT_EXE%" (
  echo [ERROR] qbit-upload.exe not found: "%QBIT_EXE%"
  exit /b 2
)

set /a TOTAL=0
set /a OK=0
set /a FAIL=0

:loop
if "%~1"=="" goto done

set /a TOTAL+=1
set "SRC=%~1"
echo.
echo [INFO] [!TOTAL!] Processing: "!SRC!"

if not exist "!SRC!\" (
  echo [ERROR] Directory not found: "!SRC!"
  set /a FAIL+=1
  shift
  goto loop
)

if defined QBIT_COMMON_ARGS (
  "%QBIT_EXE%" "!SRC!" %QBIT_COMMON_ARGS%
) else (
  "%QBIT_EXE%" "!SRC!"
)

if errorlevel 1 (
  echo [ERROR] Failed: "!SRC!"
  set /a FAIL+=1
) else (
  echo [INFO] Done: "!SRC!"
  set /a OK+=1
)

shift
goto loop

:done
echo.
echo [SUMMARY] total=!TOTAL!, success=!OK!, fail=!FAIL!
if !FAIL! gtr 0 (
  exit /b 10
)
exit /b 0

