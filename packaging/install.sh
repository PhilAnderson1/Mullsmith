#!/bin/sh

set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Gabkeep must be installed as root." >&2
    exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Release archives place install.sh and gabkeep at their root. In a source
# checkout this script remains under packaging/; accept either the make
# output (./gabkeep) or bin/gabkeep.
content_root="$script_dir"
binary="$content_root/gabkeep"
if [ ! -f "$binary" ] && [ -f "$script_dir/../go.mod" ]; then
    content_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
    binary="$content_root/gabkeep"
    if [ ! -f "$binary" ]; then
        binary="$content_root/bin/gabkeep"
    fi
fi

config_source="$content_root/configs/gabkeep.yaml"
service_source="$content_root/packaging/systemd/gabkeep.service"
readme_source="$content_root/README.md"
spec_source="$content_root/spec.md"
license_source="$content_root/LICENSE"
third_party_notices_source="$content_root/THIRD_PARTY_NOTICES.md"
third_party_licenses_source="$content_root/THIRD_PARTY_LICENSES"

for required_file in "$binary" "$config_source" "$readme_source" "$spec_source" "$license_source" "$third_party_notices_source"; do
    if [ ! -f "$required_file" ]; then
        echo "Required installation file is missing: $required_file" >&2
        exit 1
    fi
done

if [ ! -d "$third_party_licenses_source" ]; then
    echo "Required installation directory is missing: $third_party_licenses_source" >&2
    exit 1
fi

if ! getent group gabkeep >/dev/null 2>&1; then
    if command -v groupadd >/dev/null 2>&1; then
        groupadd --system gabkeep
    elif command -v addgroup >/dev/null 2>&1; then
        addgroup -S gabkeep
    else
        echo "Cannot create the gabkeep group: groupadd/addgroup not found." >&2
        exit 1
    fi
fi

if ! id gabkeep >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --gid gabkeep --home-dir /nonexistent --shell /usr/sbin/nologin gabkeep
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -D -H -G gabkeep -s /sbin/nologin gabkeep
    else
        echo "Cannot create the gabkeep user: useradd/adduser not found." >&2
        exit 1
    fi
fi

install -m 0755 "$binary" /usr/local/bin/gabkeep
install -d -o root -g gabkeep -m 0750 /etc/gabkeep
install -d -o gabkeep -g gabkeep -m 0700 /var/lib/gabkeep
install -d -o root -g root -m 0755 /usr/local/share/gabkeep
install -o root -g root -m 0644 "$readme_source" /usr/local/share/gabkeep/README.md
install -o root -g root -m 0644 "$spec_source" /usr/local/share/gabkeep/spec.md
install -o root -g root -m 0644 "$license_source" /usr/local/share/gabkeep/LICENSE
install -o root -g root -m 0644 "$third_party_notices_source" /usr/local/share/gabkeep/THIRD_PARTY_NOTICES.md
install -d -o root -g root -m 0755 /usr/local/share/gabkeep/THIRD_PARTY_LICENSES
cp -R "$third_party_licenses_source/." /usr/local/share/gabkeep/THIRD_PARTY_LICENSES/
find /usr/local/share/gabkeep/THIRD_PARTY_LICENSES -type d -exec chmod 0755 {} +
find /usr/local/share/gabkeep/THIRD_PARTY_LICENSES -type f -exec chmod 0644 {} +

install_config_if_missing() {
    source_file=$1
    destination_file=$2

    if [ -e "$destination_file" ]; then
        echo "Preserving existing $destination_file"
    else
        install -o root -g gabkeep -m 0640 "$source_file" "$destination_file"
        echo "Installed $destination_file"
    fi
}

install_config_if_missing "$config_source" /etc/gabkeep/config.yaml

# The systemd unit reads this EnvironmentFile (optional, referenced by the
# config's ${ENV_VAR} expansions). Same install-if-missing rule; it may later
# hold API keys, so owner-read/write only.
if [ -e /etc/gabkeep/env ]; then
    echo "Preserving existing /etc/gabkeep/env"
else
    # Template only (no secrets yet), so writing it plainly is safe; the
    # mode/group below is what real keys added later require.
    cat > /etc/gabkeep/env <<'EOF'
# Gabkeep runtime secrets (§9.12). /etc/gabkeep/config.yaml references these
# as ${ENV_VAR} / ${ENV_VAR:-default}. Keep this file mode 0640,
# root:gabkeep — the service reads it as the gabkeep user.
# GABKEEP_SEARCH_API_KEY=...
EOF
    chown root:gabkeep /etc/gabkeep/env
    chmod 0640 /etc/gabkeep/env
    echo "Installed /etc/gabkeep/env"
fi

if [ -f "$service_source" ] && command -v systemctl >/dev/null 2>&1; then
    install -m 0644 "$service_source" /etc/systemd/system/gabkeep.service
    systemctl daemon-reload
    echo "Installed the systemd service (not enabled or started)."
    echo ""
    echo "The unit stages TLS certificates from certbot into /run/gabkeep:"
    echo "replace YOUR.HOSTNAME in /etc/systemd/system/gabkeep.service with"
    echo "your certbot domain, or delete that ExecStartPre line when a reverse"
    echo "proxy terminates TLS (keep server.tls.enabled: false in the config)."
fi

echo "Installed Gabkeep to /usr/local/bin/gabkeep"
echo ""
echo "Edit /etc/gabkeep/config.yaml (llm.endpoint and model at minimum) and"
echo "/etc/gabkeep/env, then validate them with:"
echo "  sh -c 'set -a; . /etc/gabkeep/env; set +a; /usr/local/bin/gabkeep -config /etc/gabkeep/config.yaml -check-config'"
echo ""
echo "Create the first account with:"
echo "  sudo -u gabkeep /usr/local/bin/gabkeep -config /etc/gabkeep/config.yaml admin create-user \\"
echo "    -email you@example.org -name You -password 'a long passphrase'"
echo ""
if [ -f "$service_source" ] && command -v systemctl >/dev/null 2>&1; then
    echo "Enable and start Gabkeep with:"
    echo "  systemctl enable --now gabkeep"
    echo ""
    echo "For an upgrade where Gabkeep is already running, restart it with:"
    echo "  systemctl restart gabkeep"
fi
