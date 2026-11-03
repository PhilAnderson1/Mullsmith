#!/bin/sh

set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Mullsmith must be uninstalled as root." >&2
    exit 1
fi

if [ -e /usr/bin/mullsmith ] || [ -e /usr/lib/systemd/system/mullsmith.service ]; then
    echo "A native-package installation of Mullsmith is present." >&2
    echo "This tar uninstaller would delete configuration and data shared with it." >&2
    echo "Remove the native package with apt or dnf instead." >&2
    exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl is required to stop Mullsmith safely before uninstalling it." >&2
    exit 1
fi

# Deleting through a mounted scratchpad would empty that filesystem and then
# leave the uninstall incomplete because its mount point cannot be removed.
if [ ! -r /proc/self/mountinfo ]; then
    echo "Cannot inspect mounted filesystems; refusing a potentially partial uninstall." >&2
    exit 1
fi
mounted_paths=$(awk -v root=/var/lib/mullsmith \
    '$5 == root || index($5, root "/") == 1 { print $5 }' \
    /proc/self/mountinfo)
if [ -n "$mounted_paths" ]; then
    echo "Mounted storage exists within /var/lib/mullsmith:" >&2
    echo "$mounted_paths" >&2
    echo "Remove any data you no longer want, unmount it, then run this uninstaller again." >&2
    exit 1
fi

echo "WARNING: This will permanently uninstall Mullsmith and delete:"
echo "  - /usr/local/bin/mullsmith"
echo "  - /usr/local/share/mullsmith (documentation)"
echo "  - /etc/mullsmith (configuration and environment file)"
echo "  - /var/lib/mullsmith (database and user files at the default paths)"
echo "  - /usr/local/lib/systemd/system/mullsmith.service"
echo "  - /etc/systemd/system/mullsmith.service (if customised)"
echo "  - /etc/systemd/system/mullsmith.service.d (if present)"
echo "  - /run/mullsmith"
echo ""
echo "The configuration, chat history and user files cannot be recovered unless"
echo "you have made a backup."
echo ""
printf "Type 'uninstall' to continue: "
IFS= read -r confirmation

if [ "$confirmation" != "uninstall" ]; then
    echo "Uninstall cancelled."
    exit 0
fi

if systemctl is-active --quiet mullsmith.service; then
    if ! systemctl stop mullsmith.service; then
        echo "Mullsmith could not be stopped; no files have been deleted." >&2
        exit 1
    fi
fi
if systemctl is-active --quiet mullsmith.service; then
    echo "Mullsmith is still running; no files have been deleted." >&2
    exit 1
fi
if ! systemctl disable mullsmith.service >/dev/null 2>&1; then
    echo "Mullsmith could not be disabled; no files have been deleted." >&2
    exit 1
fi

rm -f /etc/systemd/system/mullsmith.service
rm -rf /etc/systemd/system/mullsmith.service.d
rm -f /usr/local/lib/systemd/system/mullsmith.service
rm -f /usr/local/bin/mullsmith
rm -rf /usr/local/share/mullsmith
rm -rf /etc/mullsmith
rm -rf /var/lib/mullsmith
rm -rf /run/mullsmith

systemctl daemon-reload
systemctl reset-failed mullsmith.service >/dev/null 2>&1 || true

echo ""
printf "Remove the mullsmith system user and group? [y/N] "
IFS= read -r remove_account

case "$remove_account" in
    y|Y|yes|YES|Yes)
        if id mullsmith >/dev/null 2>&1; then
            if command -v userdel >/dev/null 2>&1; then
                if ! userdel mullsmith; then
                    echo "WARNING: Could not remove the mullsmith user." >&2
                fi
            elif command -v deluser >/dev/null 2>&1; then
                if ! deluser mullsmith; then
                    echo "WARNING: Could not remove the mullsmith user." >&2
                fi
            else
                echo "WARNING: userdel/deluser not found; the mullsmith user remains." >&2
            fi
        fi

        if getent group mullsmith >/dev/null; then
            if command -v groupdel >/dev/null 2>&1; then
                if ! groupdel mullsmith; then
                    echo "WARNING: Could not remove the mullsmith group." >&2
                fi
            elif command -v delgroup >/dev/null 2>&1; then
                if ! delgroup mullsmith; then
                    echo "WARNING: Could not remove the mullsmith group." >&2
                fi
            else
                echo "WARNING: groupdel/delgroup not found; the mullsmith group remains." >&2
            fi
        fi
        ;;
    *)
        echo "Preserving the mullsmith system user and group."
        ;;
esac

echo "Mullsmith has been uninstalled."
