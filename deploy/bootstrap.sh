#!/usr/bin/env bash
# Runs ON the EC2 box: installs docker, kind, kubectl and helm, then runs
# deploy/install.sh. With "bench" it also runs the chaos benchmark and saves
# the log in results/. Safe to re-run. Extra install options pass through the
# environment (MONITORING=1, SNAPSHOTS=1, HELM_ARGS=...).
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sudo sh
  sudo usermod -aG docker "$USER"
fi
if ! command -v kind >/dev/null; then
  curl -fsSLo /tmp/kind https://github.com/kubernetes-sigs/kind/releases/latest/download/kind-linux-amd64
  sudo install /tmp/kind /usr/local/bin/kind
fi
if ! command -v kubectl >/dev/null; then
  curl -fsSLo /tmp/kubectl "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
  sudo install /tmp/kubectl /usr/local/bin/kubectl
fi
if ! command -v helm >/dev/null; then
  HELM_VERSION=${HELM_VERSION:-v3.16.3}
  curl -fsSLo /tmp/helm.tgz "https://get.helm.sh/helm-$HELM_VERSION-linux-amd64.tar.gz"
  tar -xzf /tmp/helm.tgz -C /tmp linux-amd64/helm
  sudo install /tmp/linux-amd64/helm /usr/local/bin/helm
fi
# Five kind nodes need more inotify handles than the Ubuntu default.
sudo sysctl -qw fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=512

if docker ps >/dev/null 2>&1; then export DOCKER=docker KIND=kind; else export DOCKER="sudo docker" KIND="sudo kind"; fi
deploy/install.sh

if [ "${1:-}" = "bench" ]; then
  echo "== running chaos benchmark (about 12 minutes)"
  mkdir -p results
  kubectl -n anton delete job anton-bench --ignore-not-found
  kubectl -n anton create -f deploy/bench/job.yaml
  kubectl -n anton wait --for=condition=Ready pod -l app=anton-bench --timeout=120s
  out="results/bench-$(date +%Y%m%d-%H%M%S).log"
  kubectl -n anton logs -f job/anton-bench | tee "$out"
  kubectl -n anton logs deployment/anton-operator > "${out%.log}-operator.log"
  echo "== saved $out"
fi
