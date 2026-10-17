@echo off
REM PiPER ACT tunnel: keeps an outbound SSH reverse tunnel from this Dell to the relay server, so the
REM server's :8790 reaches the local portal on 127.0.0.1:8790. Start the portal first.
REM The server address is deliberately NOT stored in the repository. Put it in
REM piper_act_tunnel.local.cmd next to this file (gitignored), containing for example:
REM     set PIPER_ACT_SERVER=user@203.0.113.7
REM or set PIPER_ACT_SERVER in the environment.
if exist "%~dp0piper_act_tunnel.local.cmd" call "%~dp0piper_act_tunnel.local.cmd"
if "%PIPER_ACT_SERVER%"=="" goto noserver
:loop
ssh -o BatchMode=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -N -R 18790:127.0.0.1:8790 %PIPER_ACT_SERVER%
echo tunnel dropped (exit %errorlevel%), reconnecting in 5s...
timeout /t 5 /nobreak >nul
goto loop
:noserver
echo PIPER_ACT_SERVER is not set. Create piper_act_tunnel.local.cmd next to this file containing:
echo     set PIPER_ACT_SERVER=user@your.server
pause
exit /b 1
