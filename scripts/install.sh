#!/usr/bin/env bash
# Goolify — One-Command Installer for Linux (Ubuntu, Debian, RHEL/Rocky/Alma, Fedora)
# Derived from Coolify (https://github.com/coollabsio/coolify, Apache License 2.0)
# Maintained by Worachote Pansanit (https://github.com/realrudragon/goolify)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/realrudragon/goolify/main/scripts/install.sh | sudo bash
#
# Optional environment variables:
#   GOOLIFY_DIR          Install directory (default: /opt/goolify)
#   GOOLIFY_REPO         GitHub owner/repo (default: realrudragon/goolify)
#   GOOLIFY_BRANCH       Git branch to install (default: main)
#   APP_PORT             External HTTP port (default: 8000)
#   APP_URL              Public URL (default: http://<server-ip>:<APP_PORT>)
#   APP_KEY              Existing base64:32-byte key (auto-generated on fresh install)
#   DB_PASSWORD          PostgreSQL password (auto-generated on fresh install)
#   ROOT_USER_EMAIL      Initial admin email (default: admin@goolify.local)
#   ROOT_USER_PASSWORD   Initial admin password (auto-generated on fresh install)

set -euo pipefail

GOOLIFY_DIR="${GOOLIFY_DIR:-/opt/goolify}"
GOOLIFY_REPO="${GOOLIFY_REPO:-realrudragon/goolify}"
GOOLIFY_BRANCH="${GOOLIFY_BRANCH:-main}"
APP_PORT="${APP_PORT:-8000}"
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

log_step() { echo -e "\n${CYAN}${BOLD}==>${NC} ${BOLD}$1${NC}"; }
log_ok()   { echo -e "${GREEN}  ✓${NC} $1"; }
log_warn() { echo -e "${YELLOW}  !${NC} $1"; }
log_err()  { echo -e "${RED}  ✗ $1${NC}" >&2; }

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  log_err "Please run this installer as root or with sudo (e.g. '| sudo -E bash')."
  exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *)
    log_err "Unsupported CPU architecture: $ARCH (expected amd64 or arm64)"
    exit 1
    ;;
esac

OS_NAME="Linux"
if [[ -f /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_NAME="${PRETTY_NAME:-$NAME}"
fi

echo -e "${CYAN}${BOLD}"
cat <<'BANNER'
   ____             _ _  __
  / ___| ___   ___ | (_)/ _|_   _
 | |  _ / _ \ / _ \| | | |_| | | |
 | |_| | (_) | (_) | | |  _| |_| |
  \____|\___/ \___/|_|_|_|  \__, |
                            |___/
BANNER
echo -e "${NC}${BOLD}Standalone Go + Rust + Astro/Vue PaaS (based on Coolify)${NC}"
echo "  OS: ${OS_NAME} (${ARCH}) | Target: ${GOOLIFY_DIR}"

# ---------------------------------------------------------------------------
# 1. Install base dependencies (git, curl, openssl, ca-certificates)
# ---------------------------------------------------------------------------
log_step "1/6 Checking system prerequisites"
install_pkg() {
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq "$@" >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q "$@" >/dev/null
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q "$@" >/dev/null
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache -q "$@" >/dev/null
  else
    log_err "Unsupported package manager. Please install: $*"
    exit 1
  fi
}

MISSING_PKGS=()
for bin in git curl openssl; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    MISSING_PKGS+=("$bin")
  fi
done
if [[ ${#MISSING_PKGS[@]} -gt 0 ]]; then
  log_warn "Installing missing packages: ${MISSING_PKGS[*]}"
  install_pkg ca-certificates "${MISSING_PKGS[@]}"
fi
log_ok "git, curl, and openssl are ready"

# ---------------------------------------------------------------------------
# 2. Install Docker Engine & Docker Compose plugin
# ---------------------------------------------------------------------------
log_step "2/6 Checking Docker & Docker Compose"
if ! command -v docker >/dev/null 2>&1; then
  log_warn "Docker not found — installing official Docker Engine..."
  curl -fsSL https://get.docker.com | sh
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now docker >/dev/null 2>&1 || true
fi

if ! docker compose version >/dev/null 2>&1; then
  log_err "Docker Compose v2 plugin ('docker compose') is required but unavailable."
  exit 1
fi
log_ok "$(docker --version) | $(docker compose version)"

# ---------------------------------------------------------------------------
# 3. Clone or update Goolify repository (without saving token to disk)
# ---------------------------------------------------------------------------
log_step "3/6 Fetching Goolify (${GOOLIFY_REPO}@${GOOLIFY_BRANCH})"
REPO_URL="https://github.com/${GOOLIFY_REPO}.git"

ASKPASS_SCRIPT=""
cleanup_askpass() {
  if [[ -n "$ASKPASS_SCRIPT" && -f "$ASKPASS_SCRIPT" ]]; then
    rm -f "$ASKPASS_SCRIPT"
  fi
}
trap cleanup_askpass EXIT

run_git() {
  if [[ -n "$TOKEN" ]]; then
    if [[ -z "$ASKPASS_SCRIPT" ]]; then
      ASKPASS_SCRIPT="$(mktemp)"
      chmod 700 "$ASKPASS_SCRIPT"
      cat >"$ASKPASS_SCRIPT" <<'EOF'
#!/bin/sh
case "$1" in
  *Username*) echo "x-access-token" ;;
  *) echo "$GOOLIFY_GIT_TOKEN" ;;
esac
EOF
    fi
    GOOLIFY_GIT_TOKEN="$TOKEN" GIT_TERMINAL_PROMPT=0 GIT_ASKPASS="$ASKPASS_SCRIPT" git "$@"
  else
    GIT_TERMINAL_PROMPT=0 git "$@"
  fi
}

if [[ -d "${GOOLIFY_DIR}/.git" ]]; then
  log_ok "Existing checkout found at ${GOOLIFY_DIR} — updating..."
  run_git -C "$GOOLIFY_DIR" fetch --depth=1 origin "$GOOLIFY_BRANCH"
  run_git -c core.fileMode=false -C "$GOOLIFY_DIR" checkout -q "$GOOLIFY_BRANCH"
  run_git -c core.fileMode=false -C "$GOOLIFY_DIR" merge --ff-only "origin/${GOOLIFY_BRANCH}"
else
  mkdir -p "$(dirname "$GOOLIFY_DIR")"
  run_git clone --depth=1 --branch "$GOOLIFY_BRANCH" "$REPO_URL" "$GOOLIFY_DIR"
fi
log_ok "Source ready at ${GOOLIFY_DIR}"

# ---------------------------------------------------------------------------
# 4. Generate secure .env on first install (never rotate existing APP_KEY)
# ---------------------------------------------------------------------------
log_step "4/6 Configuring environment (${GOOLIFY_DIR}/.env)"
ENV_FILE="${GOOLIFY_DIR}/.env"
FRESH_INSTALL=0

detect_ip() {
  local ip=""
  ip="$(curl -4 -fsS --max-time 3 https://ifconfig.me 2>/dev/null || true)"
  if [[ -z "$ip" ]]; then
    ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
  fi
  if [[ -z "$ip" ]]; then
    ip="127.0.0.1"
  fi
  echo "$ip"
}

SERVER_IP="$(detect_ip)"

if [[ -f "$ENV_FILE" ]]; then
  log_ok "Existing ${ENV_FILE} preserved (APP_KEY untouched)"
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
else
  FRESH_INSTALL=1
  GEN_APP_KEY="${APP_KEY:-base64:$(openssl rand -base64 32)}"
  GEN_DB_PASS="${DB_PASSWORD:-$(openssl rand -hex 24)}"
  GEN_EMAIL="${ROOT_USER_EMAIL:-admin@goolify.local}"
  GEN_PASS="${ROOT_USER_PASSWORD:-$(openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 20)}"
  GEN_URL="${APP_URL:-http://${SERVER_IP}:${APP_PORT}}"

  cat >"$ENV_FILE" <<EOF
# Goolify Production Environment (generated $(date -u +"%Y-%m-%dT%H:%M:%SZ"))
# IMPORTANT: Back up APP_KEY! Rotating it makes encrypted database secrets unreadable.
APP_KEY=${GEN_APP_KEY}
APP_URL=${GEN_URL}
APP_PORT=${APP_PORT}
DB_USERNAME=coolify
DB_DATABASE=coolify
DB_PASSWORD=${GEN_DB_PASS}
ROOT_USER_EMAIL=${GEN_EMAIL}
ROOT_USER_PASSWORD=${GEN_PASS}
EOF
  chmod 600 "$ENV_FILE"
  APP_URL="$GEN_URL"
  ROOT_USER_EMAIL="$GEN_EMAIL"
  ROOT_USER_PASSWORD="$GEN_PASS"
  log_ok "Generated secure ${ENV_FILE} (mode 600)"
fi

# ---------------------------------------------------------------------------
# 5. Build and launch standalone stack
# ---------------------------------------------------------------------------
log_step "5/6 Building and starting Goolify containers (first build takes ~3-8 min)"
cd "$GOOLIFY_DIR"
docker compose -f compose.yml up -d --build

log_step "6/6 Waiting for Goolify control plane health check"
HEALTH_URL="http://127.0.0.1:${APP_PORT:-8000}/api/v1/health"
READY=0
for _ in $(seq 1 60); do
  if curl -fsS --max-time 2 "$HEALTH_URL" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done

# Install convenient global 'goolify' CLI wrapper
chmod +x "${GOOLIFY_DIR}/scripts/dev" "${GOOLIFY_DIR}/scripts/install.sh" 2>/dev/null || true
cat >/usr/local/bin/goolify <<EOF
#!/usr/bin/env bash
set -euo pipefail
cd "${GOOLIFY_DIR}"
exec bash ./scripts/dev "\${@:-status}"
EOF
chmod +x /usr/local/bin/goolify
log_ok "Installed global helper command: /usr/local/bin/goolify"

echo ""
if [[ "$READY" -eq 1 ]]; then
  echo -e "${GREEN}${BOLD}Goolify is up and healthy!${NC}"
else
  log_warn "Containers started, but health check did not respond within 120s."
  echo "  Inspect startup logs with: goolify logs controlplane"
fi

echo ""
echo -e "  ${BOLD}Web UI:${NC}   ${APP_URL:-http://${SERVER_IP}:${APP_PORT:-8000}}"
if [[ "$FRESH_INSTALL" -eq 1 ]]; then
  echo -e "  ${BOLD}Email:${NC}    ${ROOT_USER_EMAIL}"
  echo -e "  ${BOLD}Password:${NC} ${ROOT_USER_PASSWORD}"
  echo -e "  ${YELLOW}Note:${NC}     Credentials & APP_KEY are saved in ${ENV_FILE} (chmod 600)."
fi
echo ""
echo "  Useful commands:"
echo "    goolify status                 # Check container health"
echo "    goolify logs                   # Follow logs"
echo "    goolify restart                # Restart stack"
echo "    goolify upgrade                # Pull latest & rebuild"
echo "    goolify cli --help             # Run internal admin CLI"
echo ""
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "Status: active"; then
  log_warn "UFW firewall is active. Open port ${APP_PORT:-8000} if needed:"
  echo "    sudo ufw allow ${APP_PORT:-8000}/tcp && sudo ufw allow 80,443/tcp"
fi
