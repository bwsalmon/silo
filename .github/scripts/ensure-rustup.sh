#!/usr/bin/env bash
set -euo pipefail

if command -v rustup >/dev/null 2>&1; then
  exit 0
fi

cargo_bin="${CARGO_HOME:-$HOME/.cargo}/bin"
if [ ! -x "$cargo_bin/rustup" ]; then
  rustup_version="1.29.3"
  case "$(uname -m)" in
    x86_64)
      rustup_target="x86_64-unknown-linux-gnu"
      rustup_sha256="b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55"
      ;;
    aarch64)
      rustup_target="aarch64-unknown-linux-gnu"
      rustup_sha256="9f608c142e0e38a78957d0661ba1dfaf0b6fb37184cfaa4b9a0aa57af6184282"
      ;;
    *)
      echo "Unsupported architecture for rustup bootstrap: $(uname -m)" >&2
      exit 1
      ;;
  esac

  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 --retry-delay 2 \
    "https://static.rust-lang.org/rustup/archive/${rustup_version}/${rustup_target}/rustup-init" \
    -o "$tmpdir/rustup-init"
  printf '%s  %s\n' "$rustup_sha256" "$tmpdir/rustup-init" | sha256sum -c -
  chmod +x "$tmpdir/rustup-init"
  "$tmpdir/rustup-init" -y --no-modify-path --default-toolchain none --profile minimal
fi

if [ -n "${GITHUB_PATH:-}" ]; then
  printf '%s\n' "$cargo_bin" >> "$GITHUB_PATH"
fi
