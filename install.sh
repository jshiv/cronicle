#!/usr/bin/env sh
# install.sh — install the cronicle binary from GitHub releases.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/jshiv/cronicle/master/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/jshiv/cronicle/master/install.sh | CRONICLE_VERSION=v0.4.0 sh
#   curl -fsSL https://raw.githubusercontent.com/jshiv/cronicle/master/install.sh | CRONICLE_INSTALL_DIR=$HOME/.local/bin sh
#
# Environment:
#   CRONICLE_VERSION      — release tag to install (default: latest)
#   CRONICLE_INSTALL_DIR  — install destination (default: /usr/local/bin if writable, else $HOME/.local/bin)

set -eu

REPO="jshiv/cronicle"
BIN_NAME="cronicle"

# ---------- helpers ----------------------------------------------------------

err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
info() { printf 'install.sh: %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || err "missing required command: $1"
}

# ---------- detect platform --------------------------------------------------

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo Linux ;;
    Darwin*) echo Darwin ;;
    MINGW*|MSYS*|CYGWIN*) echo Windows ;;
    *) err "unsupported OS: $(uname -s)" ;;
  esac
}

# Goreleaser asset names use x86_64 / arm64 / i386 — map uname -m output to those.
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo x86_64 ;;
    arm64|aarch64) echo arm64 ;;
    i386|i686) echo i386 ;;
    *) err "unsupported architecture: $(uname -m)" ;;
  esac
}

# ---------- pick install dir -------------------------------------------------

# Default to /usr/local/bin if writable (or sudo-able), else $HOME/.local/bin.
# Installer prints the dir it picked so the user can put it on PATH if needed.
pick_install_dir() {
  if [ -n "${CRONICLE_INSTALL_DIR:-}" ]; then
    printf '%s' "$CRONICLE_INSTALL_DIR"
    return
  fi
  if [ -w /usr/local/bin ] 2>/dev/null; then
    printf '%s' /usr/local/bin
    return
  fi
  if command -v sudo >/dev/null 2>&1 && [ -d /usr/local/bin ]; then
    printf '%s' /usr/local/bin
    return
  fi
  printf '%s' "$HOME/.local/bin"
}

# ---------- resolve version --------------------------------------------------

# Latest version: GitHub redirects /releases/latest to /releases/tag/<vN.N.N>.
# We follow with -L and pull the final URL's tag segment.
resolve_version() {
  if [ -n "${CRONICLE_VERSION:-}" ]; then
    printf '%s' "$CRONICLE_VERSION"
    return
  fi
  url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")
  printf '%s' "${url##*/}"
}

# ---------- main -------------------------------------------------------------

need uname
need curl
need tar

OS=$(detect_os)
ARCH=$(detect_arch)
VERSION=$(resolve_version)
INSTALL_DIR=$(pick_install_dir)

# Strip the leading "v" for the asset name, but keep the tag for the URL.
VERSION_NO_V=${VERSION#v}
ASSET="cronicle_${VERSION_NO_V}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"

info "version:     ${VERSION}"
info "platform:    ${OS}/${ARCH}"
info "install dir: ${INSTALL_DIR}"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

info "downloading ${ASSET}"
if ! curl -fsSL -o "$TMP/$ASSET" "$URL"; then
  err "download failed: $URL"
fi

tar -xzf "$TMP/$ASSET" -C "$TMP"

mkdir -p "$INSTALL_DIR"

DEST="$INSTALL_DIR/$BIN_NAME"
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP/$BIN_NAME" "$DEST"
else
  if command -v sudo >/dev/null 2>&1; then
    info "writing to ${INSTALL_DIR} requires sudo"
    sudo mv "$TMP/$BIN_NAME" "$DEST"
  else
    err "cannot write to ${INSTALL_DIR} and sudo is unavailable; set CRONICLE_INSTALL_DIR to a writable path"
  fi
fi
chmod +x "$DEST" 2>/dev/null || sudo chmod +x "$DEST"

info "installed: $DEST"

# Surface PATH advice if the chosen dir isn't already on PATH.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) info "note: ${INSTALL_DIR} is not on your PATH; add it to your shell rc:"
     info "      export PATH=\"${INSTALL_DIR}:\$PATH\""
     ;;
esac

"$DEST" --help >/dev/null 2>&1 || err "binary at $DEST is not executable"
info "done. run 'cronicle --help' to get started."
