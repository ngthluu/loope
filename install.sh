#!/bin/sh
# loope installer.
#
#   curl -fsSL https://raw.githubusercontent.com/ngthluu/loope/main/install.sh | sh
#
# Environment overrides:
#   LOOPE_VERSION       release tag to install (default: latest, e.g. v0.1.0)
#   LOOPE_INSTALL_DIR   install directory   (default: /usr/local/bin)
#
# Downloads the prebuilt binary for your OS/arch from GitHub Releases,
# verifies its checksum, and installs it. Needs: curl (or wget), tar, sha256sum
# (or shasum). loope itself also needs git, gh, and claude on your PATH at run time.

set -eu

REPO="ngthluu/loope"
BINARY="loope"

info() { printf '\033[1;34m==>\033[0m %s\n' "$1"; }
err()  { printf '\033[1;31merror:\033[0m %s\n' "$1" >&2; exit 1; }

# --- pick a downloader ------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  dl() { curl -fsSL "$1"; }
  dl_to() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  dl() { wget -qO- "$1"; }
  dl_to() { wget -qO "$2" "$1"; }
else
  err "need curl or wget installed"
fi

# --- detect OS / arch -------------------------------------------------------
os=$(uname -s)
case "$os" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) err "unsupported OS: $os (loope ships darwin and linux binaries; build from source instead)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac

# --- resolve version --------------------------------------------------------
version="${LOOPE_VERSION:-}"
if [ -z "$version" ]; then
  info "Resolving latest release..."
  version=$(dl "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
  [ -n "$version" ] || err "could not resolve latest release (has a v* tag been pushed yet?)"
fi
info "Installing ${BINARY} ${version} (${os}/${arch})"

# --- download + verify + extract -------------------------------------------
asset="${BINARY}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${version}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

info "Downloading ${asset}..."
dl_to "${base}/${asset}" "${tmp}/${asset}" || err "download failed: ${base}/${asset}"
dl_to "${base}/checksums.txt" "${tmp}/checksums.txt" || err "could not download checksums.txt"

info "Verifying checksum..."
want=$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || err "no checksum listed for ${asset}"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
else
  err "need sha256sum or shasum to verify the download"
fi
[ "$want" = "$got" ] || err "checksum mismatch for ${asset} (expected ${want}, got ${got})"

tar -xzf "${tmp}/${asset}" -C "$tmp"
[ -f "${tmp}/${BINARY}" ] || err "archive did not contain a ${BINARY} binary"
chmod +x "${tmp}/${BINARY}"

# --- install ----------------------------------------------------------------
dir="${LOOPE_INSTALL_DIR:-/usr/local/bin}"
mkdir -p "$dir" 2>/dev/null || true
if [ -w "$dir" ]; then
  mv "${tmp}/${BINARY}" "${dir}/${BINARY}"
elif command -v sudo >/dev/null 2>&1; then
  info "${dir} is not writable; using sudo"
  sudo mkdir -p "$dir"
  sudo mv "${tmp}/${BINARY}" "${dir}/${BINARY}"
else
  err "cannot write to ${dir}; re-run with LOOPE_INSTALL_DIR=\$HOME/.local/bin"
fi

info "Installed ${BINARY} to ${dir}/${BINARY}"
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) printf '\033[1;33mnote:\033[0m %s is not on your PATH — add it to use loope directly.\n' "$dir" ;;
esac
printf '\033[1;33mreminder:\033[0m loope needs git, gh (authenticated), and claude on your PATH.\n'
"${dir}/${BINARY}" -version 2>/dev/null || true
