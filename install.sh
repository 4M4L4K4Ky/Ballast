#!/bin/sh
# Instalador de Ballast. Descarga el binario de la última release de GitHub.
set -eu

REPO="4M4L4K4Ky/ballast"
BIN="ballast"

die() { printf 'error: %s\n' "$1" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "hace falta curl"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  mingw*|msys*|cygwin*) os=windows ;;
  *) die "sistema no soportado: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "arquitectura no soportada: $arch" ;;
esac

version="${BALLAST_VERSION:-}"
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
  [ -n "$version" ] || die "no se pudo obtener la última versión"
fi

ext=""
[ "$os" = "windows" ] && ext=".exe"
asset="${BIN}_${version}_${os}_${arch}${ext}"
url="https://github.com/$REPO/releases/download/$version/$asset"

tmp=$(mktemp)
printf 'descargando %s\n' "$url"
curl -fsSL -o "$tmp" "$url" || die "fallo al descargar $asset"

if [ "$os" = "windows" ]; then
  dest="${BALLAST_DIR:-$HOME/.local/bin}"
  mkdir -p "$dest"
  mv "$tmp" "$dest/$BIN.exe"
  printf 'instalado en %s\n' "$dest/$BIN.exe"
  exit 0
fi

chmod +x "$tmp"
dest="${BALLAST_DIR:-$HOME/.local/bin}"
mkdir -p "$dest"
mv "$tmp" "$dest/$BIN"
printf 'instalado en %s\n' "$dest/$BIN"

case ":$PATH:" in
  *":$dest:"*) ;;
  *) printf 'añade a tu PATH:\n  export PATH="$PATH:%s"\n' "$dest" ;;
esac

printf 'arranca con: %s\n' "$BIN"
