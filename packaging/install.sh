#!/bin/sh

set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Mullsmith must be installed as root." >&2
    exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Release archives place install.sh and mullsmith at their root. In a source
# checkout this script remains under packaging/ and make writes bin/mullsmith.
content_root="$script_dir"
binary="$content_root/mullsmith"
if [ ! -f "$binary" ] && [ -f "$script_dir/../go.mod" ]; then
    content_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
    binary="$content_root/mullsmith"
    if [ ! -f "$binary" ]; then
        binary="$content_root/bin/mullsmith"
    fi
fi

config_source="$content_root/configs/config.yaml"
endpoints_source="$content_root/configs/endpoints.yaml"
profiles_source="$content_root/configs/thinking-profiles.yaml"
service_source="$content_root/packaging/systemd/mullsmith.service"
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

if ! getent group mullsmith >/dev/null 2>&1; then
    if command -v groupadd >/dev/null 2>&1; then
        groupadd --system mullsmith
    elif command -v addgroup >/dev/null 2>&1; then
        addgroup -S mullsmith
    else
        echo "Cannot create the mullsmith group: groupadd/addgroup not found." >&2
        exit 1
    fi
fi

if ! id mullsmith >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --gid mullsmith --home-dir /nonexistent --shell /usr/sbin/nologin mullsmith
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -D -H -G mullsmith -s /sbin/nologin mullsmith
    else
        echo "Cannot create the mullsmith user: useradd/adduser not found." >&2
        exit 1
    fi
fi

install -m 0755 "$binary" /usr/local/bin/mullsmith
install -d -o root -g mullsmith -m 0750 /etc/mullsmith
install -d -o mullsmith -g mullsmith -m 0700 /var/lib/mullsmith
install -d -o root -g root -m 0755 /usr/local/share/mullsmith
install -o root -g root -m 0644 "$readme_source" /usr/local/share/mullsmith/README.md
install -o root -g root -m 0644 "$spec_source" /usr/local/share/mullsmith/spec.md
install -o root -g root -m 0644 "$license_source" /usr/local/share/mullsmith/LICENSE
install -o root -g root -m 0644 "$third_party_notices_source" /usr/local/share/mullsmith/THIRD_PARTY_NOTICES.md
install -d -o root -g root -m 0755 /usr/local/share/mullsmith/THIRD_PARTY_LICENSES
cp -R "$third_party_licenses_source/." /usr/local/share/mullsmith/THIRD_PARTY_LICENSES/
find /usr/local/share/mullsmith/THIRD_PARTY_LICENSES -type d -exec chmod 0755 {} +
find /usr/local/share/mullsmith/THIRD_PARTY_LICENSES -type f -exec chmod 0644 {} +

install_config_if_missing() {
    source_file=$1
    destination_file=$2

    if [ -e "$destination_file" ]; then
        echo "Preserving existing $destination_file"
    else
        install -o root -g mullsmith -m 0640 "$source_file" "$destination_file"
        echo "Installed $destination_file"
    fi
}

install_config_if_missing "$config_source" /etc/mullsmith/config.yaml
# The sample config names these thinking catalogues: they are the sole
# source of the endpoint and profile IDs, so they must be present (and
# may be edited freely) for the thinking selector to work.
install_config_if_missing "$endpoints_source" /etc/mullsmith/endpoints.yaml
install_config_if_missing "$profiles_source" /etc/mullsmith/thinking-profiles.yaml

# The systemd unit reads this EnvironmentFile (optional, referenced by the
# config's ${ENV_VAR} expansions). Same install-if-missing rule; it may later
# hold API keys, so owner-read/write only.
if [ -e /etc/mullsmith/env ]; then
    echo "Preserving existing /etc/mullsmith/env"
else
    # Template only (no secrets yet), so writing it plainly is safe; the
    # mode/group below is what real keys added later require.
    cat > /etc/mullsmith/env <<'EOF'
# Mullsmith runtime secrets (§9.12). /etc/mullsmith/config.yaml references these
# as ${ENV_VAR} / ${ENV_VAR:-default}. Keep this file mode 0640,
# root:mullsmith — the service reads it as the mullsmith user.
# MULLSMITH_SEARCH_API_KEY=...
EOF
    chown root:mullsmith /etc/mullsmith/env
    chmod 0640 /etc/mullsmith/env
    echo "Installed /etc/mullsmith/env"
fi

if [ -f "$service_source" ] && command -v systemctl >/dev/null 2>&1; then
    install -m 0644 "$service_source" /etc/systemd/system/mullsmith.service
    systemctl daemon-reload
    echo "Installed the systemd service (not enabled or started)."
    echo ""
    echo "TLS is OFF by default: mullsmith serves plain HTTP on loopback, which"
    echo "is fine on a private network only. To serve HTTPS with certbot"
    echo "certificates, un-comment the ExecStartPre line in"
    echo "/etc/systemd/system/mullsmith.service, replace YOUR.HOSTNAME with"
    echo "your certbot domain, and set server.tls.enabled: true in the config."
fi

echo "Installed Mullsmith to /usr/local/bin/mullsmith"
echo ""
echo "Edit /etc/mullsmith/config.yaml (llm.endpoint and model at minimum) and"
echo "/etc/mullsmith/env, then validate them with:"
echo "  sh -c 'set -a; . /etc/mullsmith/env; set +a; /usr/local/bin/mullsmith -config /etc/mullsmith/config.yaml -check-config'"
echo ""
echo "Create the first account with:"
echo "  sudo -u mullsmith /usr/local/bin/mullsmith -config /etc/mullsmith/config.yaml admin create-user \\"
echo "    -email you@example.org -name You -password 'a long passphrase'"
echo ""
if [ -f "$service_source" ] && command -v systemctl >/dev/null 2>&1; then
    echo "Enable and start Mullsmith with:"
    echo "  systemctl enable --now mullsmith"
    echo ""
    echo "For an upgrade where Mullsmith is already running, restart it with:"
    echo "  systemctl restart mullsmith"
fi
