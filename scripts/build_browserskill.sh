#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-$repo_root/artifacts/browserskill}"
# Optional Docker-style target; native builds default to the host platform.
target_platform="${2:-}"
case "$target_platform" in
  ""|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
  *) echo "Unsupported BrowserSkill target: $target_platform" >&2; exit 1 ;;
esac
# Build both ends from the same pinned protocol baseline.
source_commit="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["extension"]["source_commit"])' "$repo_root/scripts/browserskill-release.json")"
extension_version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["extension"]["version"])' "$repo_root/scripts/browserskill-release.json")"
# Native builds only. Docker runs this stage on TARGETPLATFORM so C/Rust
# dependencies and the produced daemon match the final image architecture.
host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
host_arch="$(uname -m)"
case "$host_arch" in arm64|aarch64) host_arch=arm64 ;; x86_64) host_arch=amd64 ;; esac
if [ -n "$target_platform" ] && [ "$target_platform" != "$host_os/$host_arch" ]; then
  echo "Run the BrowserSkill build on $target_platform (host is $host_os/$host_arch)" >&2
  exit 1
fi
if ! command -v cargo >/dev/null && [ -x "${CARGO_HOME:-$HOME/.cargo}/bin/cargo" ]; then
  export PATH="${CARGO_HOME:-$HOME/.cargo}/bin:$PATH"
fi
command -v cargo >/dev/null || { echo "Rust/Cargo is required to build the matching BrowserSkill daemon" >&2; exit 1; }
daemon_commit="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["daemon"]["source_commit"])' "$repo_root/scripts/browserskill-release.json")"
[ "$daemon_commit" = "$source_commit" ] || { echo "BrowserSkill daemon/extension baseline mismatch" >&2; exit 1; }
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
build_dir="$(mktemp -d /tmp/weknora-bsk-build.XXXXXX)"
trap 'rm -rf "$build_dir"' EXIT

# 只浅 fetch pinned commit（GitHub 支持按任意 SHA fetch），避免全量历史 clone
# 在不稳定代理/网络下传输大包时 TLS 中断（curl 56 GnuTLS recv error）。
# 失败时自动重试；低速僵死连接由 http.lowSpeed* 尽早掐断交给重试。
bsk_repo_url="https://github.com/Tencent/BrowserSkill.git"
git init -q "$build_dir/source"
git -C "$build_dir/source" remote add origin "$bsk_repo_url"
git -C "$build_dir/source" config http.lowSpeedLimit 1000
git -C "$build_dir/source" config http.lowSpeedTime 30
bsk_fetch_ok=""
for bsk_attempt in 1 2 3 4 5; do
  if git -C "$build_dir/source" fetch --depth 1 origin "$source_commit"; then
    bsk_fetch_ok=1
    break
  fi
  echo "git fetch $bsk_repo_url failed (attempt $bsk_attempt/5), retrying in 10s..." >&2
  sleep 10
done
[ -n "$bsk_fetch_ok" ] || { echo "git fetch failed after 5 attempts: $bsk_repo_url" >&2; exit 1; }
git -C "$build_dir/source" checkout --detach FETCH_HEAD
# 确保拿到的正是 release 基线 commit
[ "$(git -C "$build_dir/source" rev-parse HEAD)" = "$source_commit" ] \
  || { echo "fetched commit does not match pinned baseline $source_commit" >&2; exit 1; }
(
  cd "$build_dir/source"
  npx --yes pnpm@10.17.0 install --frozen-lockfile
  npx --yes pnpm@10.17.0 ext:build:zip
)
cp "$build_dir/source/apps/extension/dist/browser-skillextension-${extension_version}-chrome.zip" "$output_dir/browser-skill-weknora-${extension_version}.zip"
cp "$build_dir/source/LICENSE" "$output_dir/BrowserSkill-LICENSE"

# Build the daemon from the same pinned source; upstream has not published a
# matching CLI 0.3.1 binary.
cargo_target_dir="${CARGO_TARGET_DIR:-$build_dir/target}"
mkdir -p "$cargo_target_dir"
cargo_target_dir="$(cd "$cargo_target_dir" && pwd)"
(
  cd "$build_dir/source"
  # Use the installed toolchain; do not let the checkout's moving "stable"
  # override force a network update during each application build.
  RUSTUP_TOOLCHAIN="${RUSTUP_TOOLCHAIN:-stable}" cargo build --locked --release -p bsk --target-dir "$cargo_target_dir"
)
# Replace atomically: overwriting an executing inode can invalidate macOS code pages.
staged_binary="$(mktemp "$output_dir/.bsk-XXXXXX")"
install -m 755 "$cargo_target_dir/release/bsk" "$staged_binary"
mv -f "$staged_binary" "$output_dir/bsk"
echo "BrowserSkill $extension_version extension and daemon: $output_dir"
