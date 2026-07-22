#!/bin/sh
set -eu

repo="KCaverly/caretaker"
version="latest"
bin_dir="${CT_INSTALL_DIR:-${HOME}/.local/bin}"

usage() {
  cat <<'EOF'
Install ct from a public GitHub release.

Usage: install.sh [--version vX.Y.Z] [--bin-dir DIR]

Environment:
  CT_INSTALL_DIR  Default destination directory (default: ~/.local/bin)
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || { echo "install.sh: --version requires a value" >&2; exit 2; }
      version="$2"
      shift 2
      ;;
    --bin-dir)
      [ "$#" -ge 2 ] || { echo "install.sh: --bin-dir requires a value" >&2; exit 2; }
      bin_dir="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "install.sh: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$(uname -s)" in
  Darwin) os="Darwin" ;;
  *) echo "install.sh: ct releases currently support macOS only" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64) arch="arm64" ;;
  x86_64) arch="x86_64" ;;
  *) echo "install.sh: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

archive="ct_${os}_${arch}.tar.gz"
if [ "$version" = "latest" ]; then
  base_url="https://github.com/${repo}/releases/latest/download"
else
  case "$version" in v*) ;; *) version="v${version}" ;; esac
  base_url="https://github.com/${repo}/releases/download/${version}"
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/ct-install.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

echo "Downloading ${archive}..."
curl -fsSL "${base_url}/${archive}" -o "${tmp_dir}/${archive}"
curl -fsSL "${base_url}/checksums.txt" -o "${tmp_dir}/checksums.txt"

expected="$(awk -v file="$archive" '$2 == file { print $1 }' "${tmp_dir}/checksums.txt")"
[ -n "$expected" ] || { echo "install.sh: checksum not found for ${archive}" >&2; exit 1; }
actual="$(shasum -a 256 "${tmp_dir}/${archive}" | awk '{ print $1 }')"
[ "$actual" = "$expected" ] || { echo "install.sh: checksum verification failed" >&2; exit 1; }

tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir" ct
mkdir -p "$bin_dir"
install -m 0755 "${tmp_dir}/ct" "${bin_dir}/ct"

echo "Installed ct to ${bin_dir}/ct"
case ":${PATH}:" in
  *":${bin_dir}:"*) ;;
  *) echo "Add ${bin_dir} to PATH to run ct." ;;
esac
