#!/usr/bin/env bash
set -euo pipefail

REPO="${SMART_RESUME_REPO:-kristofferR/smarter_resume}"
VERSION="${SMART_RESUME_VERSION:-latest}"
INSTALL_DIR="${SMART_RESUME_INSTALL_DIR:-${HOME}/.claude}"
INSTALL_NAME="${SMART_RESUME_INSTALL_NAME:-smarter_resume}"

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *) echo "unsupported" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) echo "unsupported" ;;
  esac
}

download() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    echo "install.sh: curl or wget is required" >&2
    return 1
  fi
}

os_name="$(detect_os)"
arch_name="$(detect_arch)"
if [ "$os_name" = "unsupported" ] || [ "$arch_name" = "unsupported" ]; then
  echo "install.sh: unsupported platform $(uname -s)/$(uname -m)" >&2
  exit 1
fi

asset="smarter_resume_${os_name}_${arch_name}"
if [ "$VERSION" = "latest" ]; then
  base_url="https://github.com/${REPO}/releases/latest/download"
else
  base_url="https://github.com/${REPO}/releases/download/${VERSION}"
fi

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

binary=""
archive_path="${tmp_dir}/${asset}.tar.gz"
binary_path="${tmp_dir}/${asset}"

if download "${base_url}/${asset}.tar.gz" "$archive_path"; then
  tar -xzf "$archive_path" -C "$tmp_dir"
  binary="$(find "$tmp_dir" -type f \( -name smarter_resume -o -name "$asset" \) | head -n 1)"
elif download "${base_url}/${asset}" "$binary_path"; then
  binary="$binary_path"
else
  echo "install.sh: could not download ${asset} from ${base_url}" >&2
  exit 1
fi

if [ -z "$binary" ] || [ ! -f "$binary" ]; then
  echo "install.sh: downloaded asset did not contain a smarter_resume binary" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
tmp_target="$(mktemp "${INSTALL_DIR}/.${INSTALL_NAME}.XXXXXX")"
install -m 0755 "$binary" "$tmp_target"
mv -f "$tmp_target" "${INSTALL_DIR}/${INSTALL_NAME}"

echo "Installed ${INSTALL_DIR}/${INSTALL_NAME}"
echo "Alias it with:"
echo "  alias claude=\"${INSTALL_DIR}/${INSTALL_NAME}\""
