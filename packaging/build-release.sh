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
echo "Building Gabkeep $version for linux/amd64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.buildDate=$build_date" \
    -o "$work_dir/gabkeep" ./cmd/gabkeep
case $("$work_dir/gabkeep" -version) in
    "gabkeep $version "*) ;;
    *)
        echo "Built binary does not report $version" >&2
        exit 1
        ;;
esac

stage_archive() {
    archive_root="gabkeep-$version-linux-amd64"
    archive_dir="$work_dir/$archive_root"
    mkdir -p "$archive_dir/packaging/systemd"
    install -m 0755 "$work_dir/gabkeep" "$archive_dir/gabkeep"
    install -m 0755 packaging/install.sh packaging/uninstall.sh "$archive_dir/"
    install -m 0644 packaging/systemd/gabkeep.service "$archive_dir/packaging/systemd/"
    cp -R configs THIRD_PARTY_LICENSES "$archive_dir/"
    install -m 0644 README.md spec.md THIRD_PARTY_NOTICES.md LICENSE "$archive_dir/"
    tar -C "$work_dir" -czf "$artifacts/$archive_root.tar.gz" "$archive_root"
}

# Native packages do not use the interactive tarball installer/uninstaller.
# Debian purge deletes all Gabkeep configuration, chat history and accounts.
# RPM removal follows RPM convention by retaining generated state and accounts.
stage_native_payload() {
    payload="$work_dir/native"
    install -d "$payload/usr/bin" "$payload/usr/lib/systemd/system" \
        "$payload/etc/gabkeep" "$payload/usr/share/doc/gabkeep"
    install -m 0755 "$work_dir/gabkeep" "$payload/usr/bin/gabkeep"
    sed 's@ExecStart=/usr/local/bin/gabkeep@ExecStart=/usr/bin/gabkeep@' \
        packaging/systemd/gabkeep.service > "$payload/usr/lib/systemd/system/gabkeep.service"
    install -m 0640 configs/gabkeep.yaml "$payload/etc/gabkeep/config.yaml"
    install -m 0644 README.md spec.md THIRD_PARTY_NOTICES.md LICENSE "$payload/usr/share/doc/gabkeep/"
    cp -R THIRD_PARTY_LICENSES "$payload/usr/share/doc/gabkeep/THIRD_PARTY_LICENSES"
    cat > "$payload/usr/share/doc/gabkeep/PACKAGED_INSTALL.md" <<'EOF'
# Package installation

On Ubuntu, install the downloaded `.deb` with `sudo apt install ./gabkeep_*.deb`.
On AlmaLinux, install the `.rpm` with `sudo dnf install ./gabkeep-*.rpm`.

The package installs `/usr/bin/gabkeep`, creates the `gabkeep` account,
and installs a systemd unit. It does not enable or start the service. Edit
`/etc/gabkeep/config.yaml` (endpoint, paths, registration policy) and create
`/etc/gabkeep/env` with any secrets the config references through `${ENV}`
expansion, for example `GABKEEP_SEARCH_API_KEY=...` (mode 0640,
root:gabkeep).

TLS: the shipped unit stages certbot certificates into RAM-only `/run/gabkeep`
at service start. Replace `YOUR.HOSTNAME` in the unit with your certbot domain
(`sudo systemctl edit --full gabkeep`), or, if a reverse proxy terminates TLS,
remove that `ExecStartPre` line and keep `server.tls.enabled: false` in the
config.

Check the configuration and endpoint before starting:

    sudo sh -c 'set -a; . /etc/gabkeep/env 2>/dev/null; set +a; /usr/bin/gabkeep -config /etc/gabkeep/config.yaml -check-config'
    sudo systemctl enable --now gabkeep

Then create the first account:

    sudo -u gabkeep /usr/bin/gabkeep -config /etc/gabkeep/config.yaml admin create-user \
      -email you@example.org -name You -password 'a long passphrase'

Configuration is preserved on upgrade. On Debian and Ubuntu, `apt remove`
preserves configuration, the SQLite database, user files and the service
account; `apt purge` permanently deletes them. RPM has no separate purge
operation: `dnf remove` removes the packaged software while preserving generated
state under `/var/lib/gabkeep` and the service account. RPM may preserve
modified configuration as `.rpmsave` files. To remove every trace after RPM
removal, explicitly delete `/etc/gabkeep` and `/var/lib/gabkeep`, then
remove the `gabkeep` user and group.

Avoid installing a package over the tarball installation without first
migrating the latter: its `/etc/systemd/system/gabkeep.service` overrides
the package's unit, and its `/usr/local/bin/gabkeep` remains separately
installed.
EOF
}

build_deb() {
    deb_root="$work_dir/deb"
    cp -R "$payload" "$deb_root"
    install -d "$deb_root/DEBIAN"
    cat > "$deb_root/DEBIAN/control" <<EOF
Package: gabkeep
Version: $package_version
Section: web
Priority: optional
Architecture: amd64
Maintainer: Gabkeep contributors <PhilAnderson1@users.noreply.github.com>
Depends: passwd, systemd
Description: Self-hosted chat assistant for a local OpenAI-compatible LLM
 Gabkeep runs a ChatGPT-style multi-user web app in front of your own
 inference endpoint, with per-user scratchpads, governed agent tools and
 optional sandboxed code execution.
EOF
    cat > "$deb_root/DEBIAN/conffiles" <<'EOF'
/etc/gabkeep/config.yaml
EOF
    cat > "$deb_root/DEBIAN/preinst" <<'EOF'
#!/bin/sh
set -e
if ! getent group gabkeep >/dev/null; then
    groupadd --system gabkeep
fi
if ! id gabkeep >/dev/null 2>&1; then
    useradd --system --gid gabkeep --home-dir /nonexistent --shell /usr/sbin/nologin gabkeep
fi
exit 0
EOF
    cat > "$deb_root/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
    install -d -o root -g gabkeep -m 0750 /etc/gabkeep
    path=/etc/gabkeep/config.yaml
    if [ -f "$path" ] && [ ! -L "$path" ]; then
        chown root:gabkeep "$path"
        chmod 0640 "$path"
    fi
    install -d -o gabkeep -g gabkeep -m 0700 /var/lib/gabkeep
    if command -v systemctl >/dev/null 2>&1; then
        if [ -e /etc/systemd/system/gabkeep.service ]; then
            echo "WARNING: /etc/systemd/system/gabkeep.service overrides the packaged unit; check for an earlier tarball installation." >&2
        fi
        systemctl daemon-reload || true
        if [ -n "${2:-}" ] && systemctl is-active --quiet gabkeep.service; then
            systemctl try-restart gabkeep.service || true
        fi
    fi
fi
exit 0
EOF
    cat > "$deb_root/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now gabkeep.service || true
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
    rm -rf -- /etc/gabkeep /var/lib/gabkeep
    if id gabkeep >/dev/null 2>&1; then
        userdel gabkeep || echo "WARNING: Could not remove the gabkeep user." >&2
    fi
    if getent group gabkeep >/dev/null; then
        groupdel gabkeep || echo "WARNING: Could not remove the gabkeep group." >&2
    fi
fi
exit 0
EOF
    chmod 0755 "$deb_root/DEBIAN/preinst" "$deb_root/DEBIAN/postinst" \
        "$deb_root/DEBIAN/prerm" "$deb_root/DEBIAN/postrm"
    dpkg-deb --root-owner-group --build "$deb_root" \
        "$artifacts/gabkeep_${package_version}_amd64.deb"
}

build_rpm() {
    rpm_top="$work_dir/rpm"
    mkdir -p "$rpm_top/BUILD" "$rpm_top/BUILDROOT" "$rpm_top/RPMS" \
        "$rpm_top/SOURCES" "$rpm_top/SPECS" "$rpm_top/SRPMS" "$rpm_top/tmp"
    cat > "$rpm_top/SPECS/gabkeep.spec" <<EOF
Name: gabkeep
Version: $package_version
Release: 1
Summary: Self-hosted chat assistant for a local OpenAI-compatible LLM
License: MIT
URL: https://github.com/PhilAnderson1/Gabkeep
BuildArch: x86_64
AutoReqProv: no
Requires(pre): shadow-utils
Requires(post): systemd

%description
Gabkeep runs a ChatGPT-style multi-user web app in front of your own
inference endpoint, with per-user scratchpads, governed agent tools and
optional sandboxed code execution.

%prep

%build

%install
mkdir -p %{buildroot}
cp -a %{_staging}/. %{buildroot}/

%pre
if ! getent group gabkeep >/dev/null; then
    groupadd -r gabkeep
fi
if ! id gabkeep >/dev/null 2>&1; then
    useradd -r -g gabkeep -d /nonexistent -s /usr/sbin/nologin gabkeep
fi

%post
install -d -o gabkeep -g gabkeep -m 0700 /var/lib/gabkeep
if command -v systemctl >/dev/null 2>&1; then
    if [ -e /etc/systemd/system/gabkeep.service ]; then
        echo "WARNING: /etc/systemd/system/gabkeep.service overrides the packaged unit; check for an earlier tarball installation." >&2
    fi
    systemctl daemon-reload || true
fi

%posttrans
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet gabkeep.service; then
    systemctl try-restart gabkeep.service || true
fi

%preun
if [ "\$1" -eq 0 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now gabkeep.service || true
fi

%postun
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

%files
%defattr(-,root,root,-)
%dir %attr(0750,root,gabkeep) /etc/gabkeep
%config(noreplace) %attr(0640,root,gabkeep) /etc/gabkeep/config.yaml
/usr/bin/gabkeep
/usr/lib/systemd/system/gabkeep.service
%doc /usr/share/doc/gabkeep/README.md
%doc /usr/share/doc/gabkeep/spec.md
%doc /usr/share/doc/gabkeep/PACKAGED_INSTALL.md
%doc /usr/share/doc/gabkeep/THIRD_PARTY_NOTICES.md
%doc /usr/share/doc/gabkeep/THIRD_PARTY_LICENSES
%license /usr/share/doc/gabkeep/LICENSE
EOF
    rpmbuild --define "_topdir $rpm_top" --define "_tmppath $rpm_top/tmp" \
        --define "_staging $payload" \
        --define '_build_id_links none' -bb "$rpm_top/SPECS/gabkeep.spec"
    install -m 0644 "$rpm_top/RPMS/x86_64/gabkeep-$package_version-1.x86_64.rpm" \
        "$artifacts/gabkeep-$package_version-1.x86_64.rpm"
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
manifest="gabkeep-$version-SHA256SUMS.txt"
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
