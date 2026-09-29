#!/bin/bash
# aside installer — removes the download quarantine and puts `aside` on PATH.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
bin="$here/aside"

if [ ! -f "$bin" ]; then
  echo "aside binary not found next to this script." >&2
  exit 1
fi

echo "aside 설치를 시작합니다…"

# 1. Gatekeeper quarantine: downloaded binaries are blocked until cleared.
xattr -dr com.apple.quarantine "$bin" 2>/dev/null || true
chmod +x "$bin"

# 2. Pick an install location that is already on PATH.
if [ -w /opt/homebrew/bin ]; then
  dest=/opt/homebrew/bin/aside
elif [ -w /usr/local/bin ]; then
  dest=/usr/local/bin/aside
else
  dest="$HOME/.local/bin/aside"
  mkdir -p "$HOME/.local/bin"
fi

cp "$bin" "$dest"
chmod +x "$dest"
xattr -dr com.apple.quarantine "$dest" 2>/dev/null || true

echo "설치 완료: $dest"
case ":$PATH:" in
  *":$(dirname "$dest"):"*) : ;;
  *) echo "주의: $(dirname "$dest") 가 PATH에 없습니다. 셸 설정에 추가하세요." ;;
esac

cat <<'NOTE'

다음 한 가지만 해주면 됩니다 — 접근성 권한 허용:
  시스템 설정 → 개인정보 보호 및 보안 → 손쉬운 사용
  에서 이 프로그램을 실행하는 터미널 앱(Terminal / iTerm 등)을 켜세요.

실행:
  aside            # 실행
  aside doctor     # 권한·카카오톡 상태 점검

카카오톡이 꺼져 있으면 aside가 알아서 숨김으로 켭니다.
처음 실행 시 로그인 창이 뜨면 Dock에서 카카오톡을 눌러 한 번 로그인하세요.
NOTE
