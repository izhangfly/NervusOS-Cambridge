@echo off
REM Dry run: simulated arm, training video as camera. Safe to use with the arm unplugged.
cd /d "%~dp0"
start "" http://127.0.0.1:8791
"D:\Piper-CAN-Teleop\venv\Scripts\python.exe" piper_policy_portal.py --mock-arm --mock-camera %*
pause
