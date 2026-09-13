# Runtime libraries for the Android Emulator on a host that never installed
# them. The SDK, AVD and KVM device are mounted in by
# e2e/scripts/android-emulator.sh; the image holds nothing else.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    libc++1 libnss3 libxcomposite1 libxcursor1 libxi6 libxtst6 libxrandr2 \
    libasound2t64 libpulse0 libgl1 libegl1 libxkbcommon0 libxkbfile1 libxdamage1 \
    libxfixes3 libx11-6 libx11-xcb1 libxcb1 libxcb-xkb1 libxcb-icccm4 libxcb-image0 \
    libxcb-keysyms1 libxcb-render-util0 libxcb-shape0 libxcb-xinerama0 libxcb-xfixes0 \
    libxcb-randr0 libxcb-shm0 libxcb-cursor0 libfontconfig1 libdbus-1-3 libglib2.0-0 \
    libgbm1 libdrm2 libsm6 libice6 libxext6 libxrender1 procps ca-certificates \
    && rm -rf /var/lib/apt/lists/*
