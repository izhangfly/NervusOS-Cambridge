@echo off
REM ============================================================
REM  Piper CAN teleop — Windows setup
REM  Run from this folder (double-click, or from a cmd prompt).
REM ============================================================

echo.
echo  Step 0: prerequisites (do these ONCE, before this script)
echo    - Install Python 3.12 (64-bit) from python.org.
echo      CHECK "Add Python to PATH" during install.
echo    - Install the candleLight WinUSB driver with Zadig:
echo        https://zadig.akeo.ie
echo        Options ^> List All Devices ^> select "candleLight USB to CAN adapter" (USB ID 1D50:606F) ^> driver = WinUSB ^> Replace Driver.
echo    - Power the arm on and plug in the CAN stick.
echo.
pause

where python >nul 2>nul
if errorlevel 1 (
  echo [ERROR] python not found on PATH. Reinstall Python and check the PATH box.
  pause
  exit /b 1
)

echo Creating virtual environment...
python -m venv venv
if errorlevel 1 ( echo [ERROR] venv creation failed & pause & exit /b 1 )

echo Upgrading pip and installing dependencies...
venv\Scripts\python -m pip install --upgrade pip
venv\Scripts\python -m pip install -r requirements.txt
if errorlevel 1 ( echo [ERROR] dependency install failed & pause & exit /b 1 )

echo Installing vendored piper_sdk...
venv\Scripts\python -m pip install -e piper_sdk
if errorlevel 1 ( echo [ERROR] piper_sdk install failed & pause & exit /b 1 )

echo.
echo  Setup complete. Now verify the CAN link:
echo    venv\Scripts\python scripts\piper_can_check.py
echo  Expect "ARM IS ALIVE" and ~9300 frames.
echo.
pause
