#!/bin/sh
# Build the Linux x86-64 release archive and native Ubuntu/AlmaLinux packages.
set -eu

usage() {
    echo "Usage: $0 vMAJOR.MINOR.PATCH [--format all|tar|deb|rpm] [--output DIRECTORY] [--allow-dirty]" >&2
    exit 2
}

[ "$#" -ge 1 ] || usage
version=$1
shift
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || usage
package_version=${version#v}
format=all
allow_dirty=false
output_dir=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --format)
            [ "$#" -ge 2 ] || usage
            format=$2
            shift 2
            ;;
        --allow-dirty)
            allow_dirty=true
            shift
            ;;
        --output)
            [ "$#" -ge 2 ] || usage
            output_dir=$2
            shift 2
            ;;
        *) usage ;;
    esac
done
case "$format" in all|tar|deb|rpm) ;; *) usage ;; esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
cd "$source_dir"
if [ -z "$output_dir" ]; then
    output_dir="$source_dir/dist"
fi

need() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "Missing required build tool: $1" >&2
        exit 1
    }
}
for tool in git go install cp mktemp tar sha256sum; do need "$tool"; done
case "$format" in all|deb) need dpkg-deb ;; esac
case "$format" in all|rpm) need rpmbuild ;; esac

if [ "$allow_dirty" = false ] && [ -n "$(git status --porcelain)" ]; then
    echo "Working tree is not clean; commit the release or pass --allow-dirty for a test build." >&2
    exit 1
fi
if git rev-parse -q --verify "refs/tags/$version" >/dev/null 2>&1; then
    tag_commit=$(git rev-list -n 1 "$version")
    head_commit=$(git rev-parse HEAD)
    if [ "$tag_commit" != "$head_commit" ]; then
        echo "Tag $version does not point at HEAD; refusing to mislabel the build." >&2
        exit 1
    fi
fi

work_dir=$(mktemp -d "$source_dir/.release-build.XXXXXXXX")
case "$work_dir" in "$source_dir"/.release-build.*) ;; *) exit 1 ;; esac
trap 'rm -rf -- "$work_dir"' EXIT
trap 'exit 1' HUP INT TERM
artifacts="$work_dir/artifacts"
mkdir -p "$artifacts" "$work_dir/go-tmp"
# Go executes test binaries from GOTMPDIR. /tmp may be mounted noexec.
GOTMPDIR="$work_dir/go-tmp"
export GOTMPDIR

echo "Testing $(git rev-parse --short HEAD)..."
go test ./cmd/... ./internal/...
# Stamp like the Makefile does: -version prints version, commit and date.
commit=$(git rev-parse --short HEAD)
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "Building Mullsmith $version for linux/amd64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.buildDate=$build_date" \
    -o "$work_dir/mullsmith" ./cmd/mullsmith
case $("$work_dir/mullsmith" -version) in
    "mullsmith $version "*) ;;
    *)
        echo "Built binary does not report $version" >&2
        exit 1
        ;;
esac

stage_archive() {
    archive_root="mullsmith-$version-linux-amd64"
    archive_dir="$work_dir/$archive_root"
    mkdir -p "$archive_dir/packaging/systemd"
    install -m 0755 "$work_dir/mullsmith" "$archive_dir/mullsmith"
    install -m 0755 packaging/install.sh packaging/uninstall.sh "$archive_dir/"
    install -m 0644 packaging/systemd/mullsmith.service "$archive_dir/packaging/systemd/"
    cp -R configs THIRD_PARTY_LICENSES "$archive_dir/"
    install -m 0644 README.md GUIDE.md THIRD_PARTY_NOTICES.md LICENSE "$archive_dir/"
    tar -C "$work_dir" -czf "$artifacts/$archive_root.tar.gz" "$archive_root"
}

# Native packages do not use the interactive tarball installer/uninstaller.
# Debian purge deletes all Mullsmith configuration, chat history and accounts.
# RPM removal follows RPM convention by retaining generated state and accounts.
stage_native_payload() {
    payload="$work_dir/native"
    install -d "$payload/usr/bin" "$payload/usr/lib/systemd/system" \
        "$payload/etc/mullsmith" "$payload/usr/share/doc/mullsmith"
    install -m 0755 "$work_dir/mullsmith" "$payload/usr/bin/mullsmith"
    sed 's@ExecStart=/usr/local/bin/mullsmith@ExecStart=/usr/bin/mullsmith@' \
        packaging/systemd/mullsmith.service > "$payload/usr/lib/systemd/system/mullsmith.service"
    chmod 0644 "$payload/usr/lib/systemd/system/mullsmith.service"
    install -m 0640 configs/config.yaml configs/endpoints.yaml \
        configs/thinking-profiles.yaml "$payload/etc/mullsmith/"
    install -m 0644 README.md GUIDE.md THIRD_PARTY_NOTICES.md LICENSE "$payload/usr/share/doc/mullsmith/"
    cp -R THIRD_PARTY_LICENSES "$payload/usr/share/doc/mullsmith/THIRD_PARTY_LICENSES"
    cat > "$payload/usr/share/doc/mullsmith/PACKAGED_INSTALL.md" <<'EOF'
# Package installation

On Ubuntu, install the downloaded `.deb` with `sudo apt install ./mullsmith_*.deb`.
On AlmaLinux, install the `.rpm` with `sudo dnf install ./mullsmith-*.rpm`.

The package installs `/usr/bin/mullsmith`, creates the `mullsmith` account,
and enables and starts the systemd service on first installation. The default
listener is immediately reachable from the local network at
`http://SERVER-IP:3210`, with invite-only registration. Create your first account,
substituting your email, name and password:

    sudo -u mullsmith /usr/bin/mullsmith -config /etc/mullsmith/config.yaml admin create-user \
      -email you@example.org -name 'Your name' -password 'password at least 10 characters'

Open the URL and log in to check the installation. Then edit
`/etc/mullsmith/config.yaml` to configure the endpoint and models. Create
`/etc/mullsmith/env` with any secrets the config references through `${ENV}`
expansion, for example `MULLSMITH_SEARCH_API_KEY=...` (mode 0640,
root:mullsmith).

Password reset & email confirmation are optional and off by default. To turn
them on, set `email.enabled: true` in `/etc/mullsmith/config.yaml`, point
`email.base_url` at the URL your users see, and put the SMTP password in
`/etc/mullsmith/env` as `MULLSMITH_SMTP_PASSWORD` (the shipped config references
it through `${ENV}` — nothing else to change). `-check-config` below then also
dials the mail server; that probe warns but never fails, so a mail outage
cannot block restarts. See the GUIDE.md section "Password reset & email
confirmation" for relay options including unauthenticated LAN relays.

TLS: OFF by default — when started, Mullsmith listens on plain HTTP port 3210
on every IPv4/IPv6 interface so it is immediately reachable from a trusted
local network. Do not expose that port to the public internet without TLS. To
serve HTTPS with certbot certificates, un-comment the `ExecStartPre` staging
line in the unit
(`sudo systemctl edit --full mullsmith`), replace `YOUR.HOSTNAME` with your
certbot domain, and set `server.tls.enabled: true` in the config; the unit
then stages cert+key into RAM-only `/run/mullsmith` at every service start.
Certbot renewals are picked up by a deploy hook doing
`systemctl try-restart mullsmith`. If a reverse proxy terminates TLS instead,
keep `server.tls.enabled: false`, bind Mullsmith to loopback, and expose only
the proxy.

Check the configuration and endpoint after editing it, then restart:

    sudo sh -c 'set -a; . /etc/mullsmith/env 2>/dev/null; set +a; /usr/bin/mullsmith -config /etc/mullsmith/config.yaml -check-config'
    sudo systemctl restart mullsmith

Configuration is preserved on upgrade. On Debian and Ubuntu, `apt remove`
preserves configuration, the SQLite database, user files and the service
account; `apt purge` permanently deletes them. RPM has no separate purge
operation: `dnf remove` removes the packaged software while preserving generated
state under `/var/lib/mullsmith` and the service account. RPM may preserve
modified configuration as `.rpmsave` files. To remove every trace after RPM
removal, explicitly delete `/etc/mullsmith` and `/var/lib/mullsmith`, then
remove the `mullsmith` user and group.

Avoid installing a package over the tarball installation without first
migrating the latter: its `/etc/systemd/system/mullsmith.service` overrides
the package's unit, and its `/usr/local/bin/mullsmith` remains separately
installed.
EOF
}

build_deb() {
    deb_root="$work_dir/deb"
    cp -R "$payload" "$deb_root"
    install -d "$deb_root/DEBIAN"
    cat > "$deb_root/DEBIAN/control" <<EOF
Package: mullsmith
Version: $package_version
Section: web
Priority: optional
Architecture: amd64
Maintainer: Mullsmith contributors <PhilAnderson1@users.noreply.github.com>
Depends: passwd, systemd
Description: Self-hosted chat assistant for a local OpenAI-compatible LLM
 Mullsmith runs a ChatGPT-style multi-user web app in front of your own
 inference endpoint, with per-user scratchpads, governed agent tools and
 optional sandboxed code execution.
EOF
    cat > "$deb_root/DEBIAN/conffiles" <<'EOF'
/etc/mullsmith/config.yaml
/etc/mullsmith/endpoints.yaml
/etc/mullsmith/thinking-profiles.yaml
EOF
    cat > "$deb_root/DEBIAN/preinst" <<'EOF'
#!/bin/sh
set -e
if ! getent group mullsmith >/dev/null; then
    groupadd --system mullsmith
fi
if ! id mullsmith >/dev/null 2>&1; then
    useradd --system --gid mullsmith --home-dir /nonexistent --shell /usr/sbin/nologin mullsmith
fi
exit 0
EOF
    cat > "$deb_root/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
    install -d -o root -g mullsmith -m 0750 /etc/mullsmith
    for path in /etc/mullsmith/config.yaml /etc/mullsmith/endpoints.yaml /etc/mullsmith/thinking-profiles.yaml; do
        if [ -f "$path" ] && [ ! -L "$path" ]; then
            chown root:mullsmith "$path"
            chmod 0640 "$path"
        fi
    done
    install -d -o mullsmith -g mullsmith -m 0700 /var/lib/mullsmith
    if command -v systemctl >/dev/null 2>&1; then
        if [ -e /etc/systemd/system/mullsmith.service ]; then
            echo "WARNING: /etc/systemd/system/mullsmith.service overrides the packaged unit; check for an earlier tarball installation." >&2
        fi
        systemctl daemon-reload || true
        if [ -z "${2:-}" ]; then
            systemctl enable mullsmith.service || true
            systemctl start mullsmith.service || \
                echo "WARNING: Mullsmith did not start; check: systemctl status mullsmith" >&2
        elif systemctl is-active --quiet mullsmith.service; then
            systemctl try-restart mullsmith.service || true
        fi
    fi
fi
exit 0
EOF
    cat > "$deb_root/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now mullsmith.service || true
fi
exit 0
EOF
    cat > "$deb_root/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
if [ "$1" = purge ]; then
    rm -rf -- /etc/mullsmith /var/lib/mullsmith
    if id mullsmith >/dev/null 2>&1; then
        userdel mullsmith || echo "WARNING: Could not remove the mullsmith user." >&2
    fi
    if getent group mullsmith >/dev/null; then
        groupdel mullsmith || echo "WARNING: Could not remove the mullsmith group." >&2
    fi
fi
exit 0
EOF
    chmod 0755 "$deb_root/DEBIAN/preinst" "$deb_root/DEBIAN/postinst" \
        "$deb_root/DEBIAN/prerm" "$deb_root/DEBIAN/postrm"
    dpkg-deb --root-owner-group --build "$deb_root" \
        "$artifacts/mullsmith_${package_version}_amd64.deb"
}

build_rpm() {
    rpm_top="$work_dir/rpm"
    mkdir -p "$rpm_top/BUILD" "$rpm_top/BUILDROOT" "$rpm_top/RPMS" \
        "$rpm_top/SOURCES" "$rpm_top/SPECS" "$rpm_top/SRPMS" "$rpm_top/tmp"
    cat > "$rpm_top/SPECS/mullsmith.spec" <<EOF
Name: mullsmith
Version: $package_version
Release: 1
Summary: Self-hosted chat assistant for a local OpenAI-compatible LLM
License: MIT
URL: https://github.com/PhilAnderson1/Mullsmith
BuildArch: x86_64
AutoReqProv: no
Requires(pre): shadow-utils
Requires(post): systemd

%description
Mullsmith runs a ChatGPT-style multi-user web app in front of your own
inference endpoint, with per-user scratchpads, governed agent tools and
optional sandboxed code execution.

%prep

%build

%install
mkdir -p %{buildroot}
cp -a %{_staging}/. %{buildroot}/

%pre
if ! getent group mullsmith >/dev/null; then
    groupadd -r mullsmith
fi
if ! id mullsmith >/dev/null 2>&1; then
    useradd -r -g mullsmith -d /nonexistent -s /usr/sbin/nologin mullsmith
fi

%post
install -d -o mullsmith -g mullsmith -m 0700 /var/lib/mullsmith
if command -v systemctl >/dev/null 2>&1; then
    if [ -e /etc/systemd/system/mullsmith.service ]; then
        echo "WARNING: /etc/systemd/system/mullsmith.service overrides the packaged unit; check for an earlier tarball installation." >&2
    fi
    systemctl daemon-reload || true
    if [ "\$1" -eq 1 ]; then
        systemctl enable mullsmith.service || true
        systemctl start mullsmith.service || \
            echo "WARNING: Mullsmith did not start; check: systemctl status mullsmith" >&2
    fi
fi

%posttrans
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet mullsmith.service; then
    systemctl try-restart mullsmith.service || true
fi

%preun
if [ "\$1" -eq 0 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now mullsmith.service || true
fi

%postun
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

%files
%defattr(-,root,root,-)
%dir %attr(0750,root,mullsmith) /etc/mullsmith
%config(noreplace) %attr(0640,root,mullsmith) /etc/mullsmith/config.yaml
%config(noreplace) %attr(0640,root,mullsmith) /etc/mullsmith/endpoints.yaml
%config(noreplace) %attr(0640,root,mullsmith) /etc/mullsmith/thinking-profiles.yaml
/usr/bin/mullsmith
/usr/lib/systemd/system/mullsmith.service
%doc /usr/share/doc/mullsmith/README.md
%doc /usr/share/doc/mullsmith/GUIDE.md
%doc /usr/share/doc/mullsmith/PACKAGED_INSTALL.md
%doc /usr/share/doc/mullsmith/THIRD_PARTY_NOTICES.md
%doc /usr/share/doc/mullsmith/THIRD_PARTY_LICENSES
%license /usr/share/doc/mullsmith/LICENSE
EOF
    rpmbuild --define "_topdir $rpm_top" --define "_tmppath $rpm_top/tmp" \
        --define "_staging $payload" \
        --define '_build_id_links none' -bb "$rpm_top/SPECS/mullsmith.spec"
    install -m 0644 "$rpm_top/RPMS/x86_64/mullsmith-$package_version-1.x86_64.rpm" \
        "$artifacts/mullsmith-$package_version-1.x86_64.rpm"
}

case "$format" in all|tar) stage_archive ;; esac
case "$format" in all|deb|rpm) stage_native_payload ;; esac
case "$format" in all|deb) build_deb ;; esac
case "$format" in all|rpm) build_rpm ;; esac

mkdir -p "$output_dir"
for artifact in "$artifacts"/*; do
    target="$output_dir/$(basename "$artifact")"
    if [ -e "$target" ]; then
        echo "Release asset already exists: $target" >&2
        exit 1
    fi
done
manifest="mullsmith-$version-SHA256SUMS.txt"
if [ -e "$output_dir/$manifest" ]; then
    echo "Release manifest already exists: $output_dir/$manifest" >&2
    exit 1
fi
(cd "$artifacts" && sha256sum ./*) > "$work_dir/$manifest"
mv "$work_dir/$manifest" "$artifacts/$manifest"
for artifact in "$artifacts"/*; do
    mv "$artifact" "$output_dir/"
done
echo "Release assets written to $output_dir"
