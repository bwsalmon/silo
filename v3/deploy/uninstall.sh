#!/usr/bin/env bash
# uninstall.sh: remove silo v3 from a box that install.sh set up.
#
#   sudo ./uninstall.sh            # stop and remove silo; keep users, homes, state and the audit log
#   sudo ./uninstall.sh --purge    # also delete the users, their homes, /etc/silo, /var/lib/silo,
#                                  # /var/log/silo and the pinned CLIs in /opt/silo
#
# Like `silo uninstall` (design §11.4) it never reverts changes that approved
# run_as_root batches made: those are recorded in the audit log, which is
# kept unless --purge is given. Node.js (and the NodeSource repository, if
# install.sh added it) stay installed either way.
set -euo pipefail
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"

purge=0
case "${1:-}" in
"") ;;
--purge) purge=1 ;;
-h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
*) echo "uninstall.sh: unknown option $1" >&2; exit 1 ;;
esac
[ "$(id -u)" -eq 0 ] || { echo "uninstall.sh: run as root" >&2; exit 1; }

log() { printf '==> %s\n' "$*"; }

units=(silo-user.service silo-root.service silo-ui.service silo-gateway.service)

log "stopping silo"
# Agents first, so they disconnect from a gateway that is still there.
systemctl disable --now "${units[@]}" 2>/dev/null || true
# Any other agent the human or an agent started as a unit or a batch.
for u in $(systemctl list-units --plain --no-legend --all 'silo-*' 2>/dev/null | awk '{print $1}'); do
	systemctl stop "$u" 2>/dev/null || true
done

log "removing units, binary and runtime files"
for u in "${units[@]}"; do rm -f "/etc/systemd/system/$u"; done
rm -f /etc/systemd/system/silo-gatewayd.service /etc/systemd/system/silo-bridge.service
systemctl daemon-reload
systemctl reset-failed 'silo-*' 2>/dev/null || true
rm -f /usr/local/bin/silo /etc/tmpfiles.d/silo.conf
rm -rf /run/silo /usr/share/silo

if [ "$purge" -eq 1 ]; then
	log "purging users, homes, config, state, audit log and CLIs"
	for u in silo-user silo-root silo; do
		if id -u "$u" >/dev/null 2>&1; then
			pkill -KILL -u "$u" 2>/dev/null || true
			userdel "$u" 2>/dev/null || true
		fi
	done
	getent group silo-gw >/dev/null && groupdel silo-gw || true
	# The gateway makes the audit log append-only (chattr +a) where it can.
	if [ -d /var/log/silo ] && command -v chattr >/dev/null 2>&1; then
		chattr -R -a /var/log/silo 2>/dev/null || true
	fi
	rm -rf /etc/silo /var/lib/silo /var/log/silo /opt/silo
else
	cat <<EOF
Kept: users silo, silo-root and silo-user and their homes under /var/lib/silo/home,
/etc/silo, /var/lib/silo, the audit log in /var/log/silo and the CLIs in /opt/silo.
Run with --purge to remove them too.
EOF
fi
log "silo is uninstalled"
