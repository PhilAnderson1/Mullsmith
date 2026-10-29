#!/bin/sh

set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Mullsmith must be uninstalled as root." >&2
    exit 1
fi

echo "WARNING: This will permanently uninstall Mullsmith and delete:"
echo "  - /usr/local/bin/mullsmith"
echo "  - /usr/local/share/mullsmith (documentation)"
echo "  - /etc/mullsmith (configuration and environment file)"
echo "  - /var/lib/mullsmith (chat history database and all user files)"
echo "  - /etc/systemd/system/mullsmith.service"
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

if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now mullsmith.service >/dev/null 2>&1 || true
fi

rm -f /etc/systemd/system/mullsmith.service
rm -f /usr/local/bin/mullsmith
rm -rf /usr/local/share/mullsmith
rm -rf /etc/mullsmith
rm -rf /var/lib/mullsmith

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl reset-failed mullsmith.service >/dev/null 2>&1 || true
fi

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
