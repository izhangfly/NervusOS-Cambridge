@echo off
REM PiPER policy execution portal (real arm + wrist camera). Close this window to stop the portal.
cd /d "%~dp0"
start "" http://127.0.0.1:8791
"D:\Piper-CAN-Teleop\venv\Scripts\python.exe" piper_policy_portal.py %*
pause
