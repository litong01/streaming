#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK_ROOT="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-}}"
MIN_SDK="${ANDROID_MIN_SDK:-26}"

if [[ -n "${ANDROID_NDK_HOME:-}" ]]; then
  NDK_ROOT="$ANDROID_NDK_HOME"
elif [[ -n "${ANDROID_NDK_ROOT:-}" ]]; then
  NDK_ROOT="$ANDROID_NDK_ROOT"
elif [[ -n "$SDK_ROOT" && -d "$SDK_ROOT/ndk" ]]; then
  NDK_ROOT="$(ls -d "$SDK_ROOT"/ndk/* 2>/dev/null | sort | tail -n 1 || true)"
else
  NDK_ROOT=""
fi

if [[ -z "$NDK_ROOT" || ! -d "$NDK_ROOT" ]]; then
  echo "Android NDK not found. Set ANDROID_NDK_HOME or install it under ANDROID_HOME/ndk." >&2
  exit 1
fi

case "$(uname -s)" in
  Darwin) HOST_TAG="darwin-x86_64" ;;
  Linux) HOST_TAG="linux-x86_64" ;;
  *)
    echo "Unsupported build host: $(uname -s)" >&2
    exit 1
    ;;
esac

TOOLCHAIN="$NDK_ROOT/toolchains/llvm/prebuilt/$HOST_TAG"
CC="$TOOLCHAIN/bin/aarch64-linux-android${MIN_SDK}-clang"
if [[ ! -x "$CC" ]]; then
  echo "Android ARM64 compiler not found: $CC" >&2
  exit 1
fi

OUTPUT_DIR="$ROOT_DIR/app/build/generated/jniLibs/arm64-v8a"
mkdir -p "$OUTPUT_DIR"

android_build() {
  local src="$1"
  local out="$2"
  local extra_ldflags="${3:-}"
  (
    cd "$src"
    CGO_ENABLED=1 \
    GOOS=android \
    GOARCH=arm64 \
    CC="$CC" \
    go build -buildmode=pie -trimpath -buildvcs=false -ldflags="-s -w ${extra_ldflags}" -o "$out" .
  )
}

android_build "$ROOT_DIR" "$OUTPUT_DIR/libstreaming.so"
echo "Built Android Go server: $OUTPUT_DIR/libstreaming.so"

GO2RTC_VERSION="${GO2RTC_VERSION:-v1.9.14}"
GO2RTC_SRC="$ROOT_DIR/build/go2rtc-${GO2RTC_VERSION}"
if [[ ! -d "$GO2RTC_SRC/.git" ]]; then
  rm -rf "$GO2RTC_SRC"
  git clone --depth 1 --branch "$GO2RTC_VERSION" https://github.com/AlexxIT/go2rtc.git "$GO2RTC_SRC"
fi
# ALSA/V4L2 pull Linux C sources that do not cross-compile for Android.
if grep -q 'github.com/AlexxIT/go2rtc/internal/alsa' "$GO2RTC_SRC/main.go"; then
  git -C "$GO2RTC_SRC" apply "$ROOT_DIR/scripts/go2rtc-android.patch"
fi
android_build "$GO2RTC_SRC" "$OUTPUT_DIR/libgo2rtc.so" "-checklinkname=0"
echo "Built Android go2rtc: $OUTPUT_DIR/libgo2rtc.so"
