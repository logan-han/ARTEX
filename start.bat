@echo off
rem Switch the console to UTF-8; otherwise non-ASCII text is garbled in GBK terminals.
chcp 65001 >nul 2>&1
rem ARTEX supervisor start script (Windows)
rem
rem Usage:
rem   start.bat                  run in the foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      extra arguments are passed through to artex unchanged
rem
rem It does one thing only: run artex.exe, and when the process exits, use the exit code to decide whether to start it again.
rem
rem   0      normal stop by the user   -> leave the loop
rem   75     program requested restart -> rerun immediately ("One-click update" or "Roll back" was clicked in the UI)
rem   other  crash                     -> rerun after a backoff (1->2->4... up to 60 seconds)
rem
rem Downloading, SHA256 verification and swapping the binary are not done here; artex does all of it itself at startup
rem (the selfupdate package). The script stays dead simple; see the notes at the top of start.sh for details.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] cannot find the executable %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] exited normally
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem The update/rollback is staged: on rerun, artex completes the swap at startup.
	echo [artex] restart requested, applying the new version...
	set /a delay=1
	goto loop
)

echo [artex] exited abnormally ^(code=!code!^), restarting in !delay!s 1>&2
rem timeout fails in a redirected console, so fall back to ping (a delay of N seconds needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
