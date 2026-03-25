#!/usr/bin/env bash
set -euo pipefail

REPO="${1:-}"
TAG="${2:-latest}"
INSTALL_ROOT="/opt/anki-relay"
SERVICE_NAME="anki-relay.service"
ENV_FILE="/etc/anki-relay/anki-relay.env"
ASSET_NAME="anki-relay-linux-amd64.tar.gz"

if [[ -z "${REPO}" ]]; then
  echo "用法: sudo bash install-from-github.sh <github-owner/repo> [tag|latest]"
  exit 1
fi

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "缺少命令: $1"
    exit 1
  fi
}

require_cmd curl
require_cmd tar
require_cmd python3
require_cmd systemctl
require_cmd install

ARCH="$(uname -m)"
if [[ "${ARCH}" != "x86_64" && "${ARCH}" != "amd64" ]]; then
  echo "当前脚本只内置了 linux-amd64 包，当前架构为: ${ARCH}"
  exit 1
fi

resolve_latest_tag() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])'
}

if [[ "${TAG}" == "latest" ]]; then
  TAG="$(resolve_latest_tag)"
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET_NAME}"

echo "==> 下载发布包: ${DOWNLOAD_URL}"
curl -fL "${DOWNLOAD_URL}" -o "${TMP_DIR}/${ASSET_NAME}"

echo "==> 解压发布包"
tar -xzf "${TMP_DIR}/${ASSET_NAME}" -C "${TMP_DIR}"

if ! id -u anki-relay >/dev/null 2>&1; then
  echo "==> 创建系统用户 anki-relay"
  useradd --system --home "${INSTALL_ROOT}" --shell /usr/sbin/nologin anki-relay
fi

echo "==> 停止旧服务（如存在）"
systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || true

echo "==> 安装文件"
mkdir -p "${INSTALL_ROOT}"
mkdir -p /etc/anki-relay

install -m 0755 "${TMP_DIR}/anki-relay-server" "${INSTALL_ROOT}/anki-relay-server"
install -m 0644 "${TMP_DIR}/anki-relay.service" "/etc/systemd/system/${SERVICE_NAME}"

if [[ ! -f "${ENV_FILE}" ]]; then
  cat > "${ENV_FILE}" <<'EOF'
LISTEN_ADDR=:39461
RELAY_AUTH_TOKEN=anki_private_pair_token_v1
RELAY_ALLOWED_ROOM_ID=anki_private_pair_room
MAX_LINE_BYTES=1048576
EOF
fi

chown -R anki-relay:anki-relay "${INSTALL_ROOT}" /etc/anki-relay

echo "==> 重载并启动服务"
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"

echo
echo "==> 服务状态"
systemctl --no-pager --full status "${SERVICE_NAME}" || true

echo
echo "==> 当前环境文件: ${ENV_FILE}"
cat "${ENV_FILE}"

echo
echo "==> 提醒"
echo "1. 请确认云安全组放行 TCP 39461"
echo "2. 若启用了 ufw，请执行: sudo ufw allow 39461/tcp"
echo "3. 若服务器还跑着代理服务，只要它不占用 39461，即可共存"