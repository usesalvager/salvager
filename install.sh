#!/bin/sh
# install.sh — Salvager installer
#
# Downloads the prebuilt binary for your OS/arch from GitHub Releases, verifies
# its SHA-256 checksum, and installs it onto your PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/usesalvager/salvager/main/install.sh | sh
#
# Environment overrides:
#   SALVAGER_VERSION       install a specific version (e.g. v1.1.0); default: latest
#   SALVAGER_INSTALL_DIR   install into this dir; default: /usr/local/bin if
#                          writable, otherwise $HOME/.local/bin
#
# What it does NOT do: no telemetry, no phone-home, no sudo, no edits to your
# shell rc files. It installs one checksum-verified binary. That is all.

set -eu

REPO="usesalvager/salvager"
BIN_NAME="salvager"

# --- output helpers --------------------------------------------------------
# Everything goes to stderr so `curl … | sh` keeps stdout clean.
info() { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- pick a downloader -----------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  DL=curl
elif command -v wget >/dev/null 2>&1; then
  DL=wget
else
  die "need curl or wget on PATH"
fi

fetch() { # $1 url -> stdout
  if [ "$DL" = curl ]; then curl -fsSL "$1"; else wget -qO- "$1"; fi
}
download() { # $1 url, $2 dest
  if [ "$DL" = curl ]; then curl -fsSL -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}

# --- pick a checksum tool --------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
  SHACMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHACMD="shasum -a 256"
else
  die "need sha256sum or shasum for checksum verification"
fi

# --- detect OS / arch and map to release naming ----------------------------
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)  GOOS=linux ;;
  Darwin) GOOS=darwin ;;
  *) die "unsupported OS: $os (supported: Linux, Darwin)" ;;
esac
case "$arch" in
  x86_64|amd64)  GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) die "unsupported architecture: $arch (supported: x86_64/amd64, aarch64/arm64)" ;;
esac

# --- resolve version -------------------------------------------------------
VERSION="${SALVAGER_VERSION:-}"
if [ -z "$VERSION" ]; then
  info "Resolving latest version…"
  VERSION=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -m1 '"tag_name"' \
    | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
  [ -n "$VERSION" ] || die "could not resolve latest version (GitHub API rate limit? set SALVAGER_VERSION to pin a version)"
fi

ASSET="${BIN_NAME}-${VERSION}-${GOOS}-${GOARCH}"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"

info "salvager ${VERSION} — ${GOOS}/${GOARCH}"

# --- temp workspace, cleaned up no matter how we exit ----------------------
TMP=$(mktemp -d 2>/dev/null || mktemp -d -t salvager)
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

# --- download binary + its checksum ----------------------------------------
info "Downloading ${ASSET}…"
download "${BASE}/${ASSET}"        "$TMP/$ASSET"        || die "download failed: ${BASE}/${ASSET}"
download "${BASE}/${ASSET}.sha256" "$TMP/$ASSET.sha256" || die "checksum download failed: ${BASE}/${ASSET}.sha256"

# --- verify checksum BEFORE installing -------------------------------------
# The .sha256 file names the asset; verify inside $TMP so the name matches.
info "Verifying checksum…"
if ! ( cd "$TMP" && $SHACMD -c "$ASSET.sha256" >/dev/null 2>&1 ); then
  die "checksum verification FAILED — aborting. Nothing was installed; download discarded."
fi

# --- choose install dir ----------------------------------------------------
if [ -n "${SALVAGER_INSTALL_DIR:-}" ]; then
  INSTALL_DIR="$SALVAGER_INSTALL_DIR"
elif [ -w /usr/local/bin ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="$HOME/.local/bin"
fi

mkdir -p "$INSTALL_DIR" 2>/dev/null || die "cannot create install dir: $INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "install dir not writable: $INSTALL_DIR
(set SALVAGER_INSTALL_DIR to a writable location, or create $INSTALL_DIR yourself — this installer never uses sudo)"

# --- install ---------------------------------------------------------------
DEST="$INSTALL_DIR/$BIN_NAME"
chmod +x "$TMP/$ASSET"
# mv first (fast, same-fs); fall back to cp for cross-filesystem moves.
mv "$TMP/$ASSET" "$DEST" 2>/dev/null || cp "$TMP/$ASSET" "$DEST" || die "install failed: cannot write $DEST"
chmod +x "$DEST"

# --- verify it runs and matches the version we asked for -------------------
got=$("$DEST" --version 2>/dev/null) || die "installed binary failed to run: $DEST"
case "$got" in
  *"$VERSION"*) ;;
  *) die "version mismatch: expected $VERSION, got '$got'" ;;
esac

info ""
info "Installed ${got}"
info "  → ${DEST}"

# --- PATH check ------------------------------------------------------------
case ":$PATH:" in
  *":$INSTALL_DIR:"*) info "  → verify with: ${BIN_NAME} --version" ;;
  *)
    info ""
    info "NOTE: ${INSTALL_DIR} is not on your PATH."
    info "  Add it to your shell, e.g.:  export PATH=\"${INSTALL_DIR}:\$PATH\""
    info "  Or run directly:             ${DEST} --version"
    ;;
esac
