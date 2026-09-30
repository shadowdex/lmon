#!/bin/sh
# lmon installer. Downloads a release archive from GitHub, checks its SHA-256
# against the release's checksums.txt, and installs the binary. No sudo needed.
#
#   curl -fsSL https://raw.githubusercontent.com/shadowdex/lmon/main/install.sh | sh
#
# Environment:
#   LMON_VERSION      tag to install, e.g. v0.1.0 or 0.1.0 (default: latest release;
#                     pre-releases must be requested explicitly)
#   LMON_INSTALL_DIR  where to put the binary (default: $HOME/.local/bin)
#   LMON_DRY_RUN=1    print what would be downloaded and exit
#   LMON_REPO         owner/name on GitHub (default: shadowdex/lmon)
#   LMON_BASE_URL     replaces https://github.com/$LMON_REPO (mirrors, tests)
#
# Everything lives inside main(), called on the last line, so a download that is
# cut off halfway runs nothing instead of a partial script.

set -eu

say() { printf 'lmon-install: %s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

main() {
  repo=${LMON_REPO:-shadowdex/lmon}
  base=${LMON_BASE_URL:-https://github.com/$repo}
  home=${HOME:-}
  dir=${LMON_INSTALL_DIR:-${home:+$home/.local/bin}}
  [ -n "$dir" ] || die "HOME is not set; set LMON_INSTALL_DIR"

  command -v curl >/dev/null 2>&1 || die "curl is required"

  # --- platform ---
  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*)
      die "this script is for macOS and Linux. On Windows, download the .zip from $base/releases or run: go install github.com/$repo@latest" ;;
    *) die "unsupported OS '$(uname -s)'. Download an archive from $base/releases or run: go install github.com/$repo@latest" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture '$(uname -m)'. Download an archive from $base/releases or run: go install github.com/$repo@latest" ;;
  esac
  # A shell running under Rosetta reports x86_64 on an Apple Silicon Mac; the
  # native build is what you want.
  if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
     [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
    arch=arm64
  fi

  # --- version ---
  if [ -n "${LMON_VERSION:-}" ]; then
    tag=v${LMON_VERSION#v}
  else
    # /releases/latest redirects to /releases/tag/<tag>; this avoids the API's
    # rate limits and any JSON parsing. It skips pre-releases by design.
    url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$base/releases/latest") ||
      die "could not find a release at $base/releases/latest (is there a published release yet?)"
    tag=${url##*/}
    # With only pre-releases (or drafts) published, GitHub redirects to the
    # /releases list instead of a tag page.
    case "$url" in
      */releases/tag/*) ;;
      *) die "no stable release is published at $base/releases. Pre-releases are only installed when requested, e.g.: LMON_VERSION=v0.1.0-rc1" ;;
    esac
  fi
  case "$tag" in v[0-9]*) ;; *) die "unexpected release tag '$tag'" ;; esac
  ver=${tag#v}

  archive="lmon_${ver}_${os}_${arch}.tar.gz"
  download="$base/releases/download/$tag"

  if [ -n "${LMON_DRY_RUN:-}" ]; then
    say "would install $tag for $os/$arch from $download/$archive into $dir"
    return 0
  fi

  tmp=$(mktemp -d) || die "could not create a temporary directory"
  trap 'rm -rf "$tmp"' EXIT HUP INT TERM

  say "downloading lmon $tag ($os/$arch)"
  curl -fsSL --retry 3 -o "$tmp/$archive" "$download/$archive" ||
    die "download failed: $download/$archive (no such release or platform?)"
  curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$download/checksums.txt" ||
    die "download failed: $download/checksums.txt"

  # --- verify ---
  want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
  [ -n "$want" ] || die "$archive is not listed in checksums.txt"
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
  elif command -v shasum >/dev/null 2>&1; then
    got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
  elif command -v openssl >/dev/null 2>&1; then
    got=$(openssl dgst -sha256 "$tmp/$archive" | awk '{ print $NF }')
  else
    die "cannot verify the download: need sha256sum, shasum or openssl"
  fi
  [ "$got" = "$want" ] || die "checksum mismatch for $archive (expected $want, got $got); not installing"

  # --- install ---
  tar -xzf "$tmp/$archive" -C "$tmp" lmon || die "could not unpack $archive"
  mkdir -p "$dir" || die "cannot create $dir; set LMON_INSTALL_DIR to a writable directory"
  # Copy beside the destination, then rename: replacing a binary that is
  # currently running (an upgrade) is safe, and an interrupted install never
  # leaves a half-written lmon behind.
  staged="$dir/.lmon.new.$$"
  cp "$tmp/lmon" "$staged" || die "cannot write to $dir; set LMON_INSTALL_DIR to a writable directory"
  chmod 755 "$staged"
  mv -f "$staged" "$dir/lmon" || { rm -f "$staged"; die "cannot install into $dir"; }

  say "installed $dir/lmon"
  "$dir/lmon" version >&2 || say "warning: the installed binary did not run"
  case ":${PATH:-}:" in
    *":$dir:"*) ;;
    *) say "$dir is not on your PATH. Add it, e.g.:  export PATH=\"$dir:\$PATH\"" ;;
  esac
}

main "$@"
