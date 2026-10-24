#!/usr/bin/env bash
# Build a current libcamera (IPU6 + software ISP) into /usr/local, for the Surface Pro 9 cameras.
set -e
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git meson ninja-build pkg-config python3-yaml python3-ply python3-jinja2 \
  libyaml-dev libgnutls28-dev libudev-dev libevent-dev libdrm-dev libjpeg-dev libgstreamer1.0-dev \
  libgstreamer-plugins-base1.0-dev gstreamer1.0-tools gstreamer1.0-plugins-good v4l2loopback-dkms v4l2loopback-utils \
  libdw-dev libunwind-dev openssl 
cd ~
cd ~/libcamera-src && rm -rf build
meson setup build --prefix=/usr/local -Dpipelines=simple,uvcvideo -Dipas=softisp -Dgstreamer=enabled \
  -Dv4l2=enabled -Dcam=enabled -Dqcam=disabled -Dlc-compliance=disabled -Ddocumentation=disabled -Dtest=false 
ninja -C build 
sudo ninja -C build install 
sudo ldconfig
echo BUILT
