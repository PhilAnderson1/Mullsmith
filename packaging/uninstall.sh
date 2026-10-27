#!/bin/sh

set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Gabkeep must be uninstalled as root." >&2
    exit 1
fi

echo "WARNING: This will permanently uninstall Gabkeep and delete:"
echo "  - /usr/local/bin/gabkeep"
echo "  - /usr/local/share/gabkeep (documentation)"
echo "  - /etc/gabkeep (configuration and environment file)"
echo "  - /var/lib/gabkeep (chat history database and all user files)"
echo "  - /etc/systemd/system/gabkeep.service"
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
    systemctl disable --now gabkeep.service >/dev/null 2>&1 || true
fi

rm -f /etc/systemd/system/gabkeep.service
rm -f /usr/local/bin/gabkeep
rm -rf /usr/local/share/gabkeep
rm -rf /etc/gabkeep
rm -rf /var/lib/gabkeep

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl reset-failed gabkeep.service >/dev/null 2>&1 || true
fi

echo ""
printf "Remove the gabkeep system user and group? [y/N] "
IFS= read -r remove_account

case "$remove_account" in
    y|Y|yes|YES|Yes)
        if id gabkeep >/dev/null 2>&1; then
            if command -v userdel >/dev/null 2>&1; then
                if ! userdel gabkeep; then
                    echo "WARNING: Could not remove the gabkeep user." >&2
                fi
            elif command -v deluser >/dev/null 2>&1; then
                if ! deluser gabkeep; then
                    echo "WARNING: Could not remove the gabkeep user." >&2
                fi
            else
                echo "WARNING: userdel/deluser not found; the gabkeep user remains." >&2
            fi
        fi

        if getent group gabkeep >/dev/null; then
            if command -v groupdel >/dev/null 2>&1; then
                if ! groupdel gabkeep; then
                    echo "WARNING: Could not remove the gabkeep group." >&2
                fi
            elif command -v delgroup >/dev/null 2>&1; then
                if ! delgroup gabkeep; then
                    echo "WARNING: Could not remove the gabkeep group." >&2
                fi
            else
                echo "WARNING: groupdel/delgroup not found; the gabkeep group remains." >&2
            fi
        fi
        ;;
    *)
        echo "Preserving the gabkeep system user and group."
        ;;
esac

echo "Gabkeep has been uninstalled."
