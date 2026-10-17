@echo off
REM PiPER ACT - unified portal (dashboard + data collection + policy execution).
REM Local: http://127.0.0.1:8790   Remote (when the tunnel is up): http://<relay server>:8790 (see piper_act_tunnel.bat)
cd /d "%~dp0"
start "" http://127.0.0.1:8790
"D:\Piper-CAN-Teleop\venv\Scripts\python.exe" piper_act_portal.py %*
pause
