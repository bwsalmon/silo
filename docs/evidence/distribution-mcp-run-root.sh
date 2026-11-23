#!/bin/bash
set -euo pipefail
runtime_root=/var/tmp/silo-task110-clean-root
mount --make-rprivate /
mount --bind "$runtime_root" "$runtime_root"
mount --rbind /dev "$runtime_root/dev"
mount --rbind /sys "$runtime_root/sys"
mount -t proc proc "$runtime_root/proc"
mkdir "$runtime_root/.old-root"
cd "$runtime_root"
pivot_root . .old-root
cd /
umount -l /.old-root
rmdir /.old-root
test ! -e /usr/bin/bwrap
test ! -e /home/debian/work
test ! -e /home/debian/.cargo
test ! -e /usr/bin/cargo
sudo -u debian env -i HOME=/home/debian PATH=/usr/sbin:/usr/bin:/sbin:/bin sh -c 'id; cat /proc/self/status; unshare -Ur true' > /evidence/runtime-profile.txt
sudo -u debian env -i HOME=/home/debian PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C.UTF-8 PYTHONDONTWRITEBYTECODE=1 python3 /acceptance/scripts/qualify-bundle.py --archive /candidate/silo-0.1.0-linux-x86_64.tar.xz --sha256 c5ae6f81f303ae674ca038d71267c23df8a7109e900b9bb4eb3d63e82071035a --reports /evidence/qualification
