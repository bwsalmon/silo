#!/bin/bash
set -euo pipefail
runtime_root=/var/tmp/silo-task110-clean-root
test ! -e "$runtime_root"
sudo debootstrap --variant=minbase bookworm "$runtime_root" http://deb.debian.org/debian
sudo install -d -m 0755 "$runtime_root/usr/local/share/ca-certificates"
sudo install -m 0644 /usr/local/share/ca-certificates/grain-ca.crt "$runtime_root/usr/local/share/ca-certificates/grain-ca.crt"
sudo chroot "$runtime_root" apt-get update
sudo chroot "$runtime_root" apt-get install -y --no-install-recommends ca-certificates sudo python3 libcap2 binutils gcc libc6-dev musl-tools skopeo umoci openssl busybox-static e2fsprogs util-linux procps containernetworking-plugins iptables
sudo chroot "$runtime_root" update-ca-certificates
sudo chroot "$runtime_root" useradd --uid 1000 --user-group --create-home --shell /bin/bash debian
sudo install -d -m 0750 "$runtime_root/etc/sudoers.d"
printf 'debian ALL=(ALL) NOPASSWD: ALL\n' | sudo tee "$runtime_root/etc/sudoers.d/task110-disposable" >/dev/null
sudo chmod 0440 "$runtime_root/etc/sudoers.d/task110-disposable"
sudo chroot "$runtime_root" dpkg-query -W > /home/debian/mcp-clean-packages.tsv
sudo chroot "$runtime_root" sh -c '! command -v bwrap && ! command -v cargo && ! command -v rustc'
sudo install -d -o 1000 -g 1000 -m 0700 "$runtime_root/evidence"
sudo install -d -m 0755 "$runtime_root/acceptance/scripts" "$runtime_root/acceptance/tests/fixtures" "$runtime_root/candidate"
sudo cp /home/debian/work/scripts/{qualify-bundle.py,validate-bundle.py,bundle_common.py} "$runtime_root/acceptance/scripts/"
sudo cp /home/debian/work/tests/*.py "$runtime_root/acceptance/tests/"
sudo cp /home/debian/work/tests/fixtures/*.c "$runtime_root/acceptance/tests/fixtures/"
echo "Prepared $runtime_root"
