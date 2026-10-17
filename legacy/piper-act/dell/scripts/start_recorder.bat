@echo off
title PiPER recorder  -  press Ctrl+C here to stop (do NOT just close the window)
cd /d D:\Piper-CAN-Teleop\scripts
echo.
echo  PiPER recorder at http://127.0.0.1:8790
echo  Stop with Ctrl+C in this window: that saves cleanly and releases the arm.
echo  Closing the window instead can cut off a take that is still saving.
echo.
D:\Piper-CAN-Teleop\venv\Scripts\python.exe piper_record_portal.py --dataset piper_pick_place --camera "wrist=Dabai DC1" --task "pick up the white block and place it on the red area" --port 8790 --target 50
echo.
echo  Recorder stopped.
pause