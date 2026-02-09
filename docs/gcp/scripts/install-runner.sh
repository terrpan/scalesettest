#!/bin/bash
# install-runner.sh -- Packer provisioner script for building GCP runner images.
#
# Installs on Ubuntu 24.04 LTS:
#   - System tools (curl, jq, git, unzip, ca-certificates)
#   - Docker CE from the official Docker APT repository
#   - GitHub Actions runner agent
#   - scaleset-runner systemd service
#
# Variables:
#   RUNNER_VERSION  -- GitHub Actions runner version (e.g. "2.321.0")
set -euo pipefail

RUNNER_VERSION="${RUNNER_VERSION:?RUNNER_VERSION must be set}"
RUNNER_HOME="/home/runner"
RUNNER_USER="runner"

# ---------------------------------------------------------------------------
# System packages
# ---------------------------------------------------------------------------
echo ">>> Installing system packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y \
  curl \
  jq \
  git \
  unzip \
  ca-certificates \
  gnupg \
  lsb-release

# ---------------------------------------------------------------------------
# Docker CE
# ---------------------------------------------------------------------------
echo ">>> Installing Docker CE"
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | \
  gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg

echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
  https://download.docker.com/linux/ubuntu \
  $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list

apt-get update -y
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin

# ---------------------------------------------------------------------------
# Runner user
# ---------------------------------------------------------------------------
echo ">>> Creating runner user"
useradd -m -d "$RUNNER_HOME" -s /bin/bash "$RUNNER_USER"
usermod -aG docker "$RUNNER_USER"

# ---------------------------------------------------------------------------
# GitHub Actions runner agent
# ---------------------------------------------------------------------------
echo ">>> Installing GitHub Actions runner ${RUNNER_VERSION}"
RUNNER_ARCH="x64"
RUNNER_TARBALL="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
RUNNER_URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${RUNNER_TARBALL}"

curl -fsSL -o "/tmp/${RUNNER_TARBALL}" "$RUNNER_URL"
tar -xzf "/tmp/${RUNNER_TARBALL}" -C "$RUNNER_HOME"
rm -f "/tmp/${RUNNER_TARBALL}"

chown -R "${RUNNER_USER}:${RUNNER_USER}" "$RUNNER_HOME"

# Install runner dependencies (dotnet, etc.)
"${RUNNER_HOME}/bin/installdependencies.sh"

# ---------------------------------------------------------------------------
# Startup script & systemd service
# ---------------------------------------------------------------------------
echo ">>> Installing startup script and systemd service"
install -d -m 0755 /opt/scaleset
install -m 0755 /tmp/startup.sh /opt/scaleset/startup.sh

cat > /etc/systemd/system/scaleset-runner.service <<'UNIT'
[Unit]
Description=GitHub Actions Runner (scaleset)
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=exec
ExecStart=/opt/scaleset/startup.sh
Restart=no
User=root

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable scaleset-runner.service

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
echo ">>> Cleaning up"
apt-get clean
rm -rf /var/lib/apt/lists/*

echo ">>> Done"
