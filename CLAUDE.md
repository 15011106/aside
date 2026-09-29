# aside

DM을 Agent CLI 세션처럼 보이게 표시하는 터미널 메신저. macOS 카카오톡을 접근성(AX)
트리로 읽고 조작한다. UI 라벨은 한/영 겸용으로 매칭하고, **Go 단일 바이너리**(Swift
브릿지 정적 링크)로 빌드 — 런타임 의존성 없이 파일 하나로 배포된다.

## 빌드·실행

```bash
make build     # swiftc로 bridge를 .a로 컴파일 → cgo 링크
./aside doctor # 권한·카톡 상태 점검
./aside ls 10  # 대화 목록 (읽기 전용, 안전)
./aside        # TUI — ↑/↓·Enter로 방 선택, `/`로 이름 필터
./aside read 침용   # 이름 일부만으로 해석: 창 없으면 열어서(읽음처리됨) 읽음
```

- 요구사항: 터미널 앱에 손쉬운 사용 권한만. 카톡이 꺼져 있으면 aside가 숨김
  상태로 자동 실행(EnsureAppRunning)하고, 준비되면 항상 숨김(Cmd+H 동급)으로
  둔다 — 창은 존재하지만 화면엔 절대 안 보임. 로그인이 필요하면 즉시 감지해서
  "Dock에서 열어 로그인" 안내. 빌드에만 Xcode CLT 필요.
- Swift 브릿지 수정 후에는 반드시 `make build`. Makefile이 .a 해시를
  `-X main.bridgeID`로 박아 Go 빌드 캐시를 깬다 — **Go는 외부 링크 라이브러리
  내용을 해시하지 않아서** 이 장치 없이는 브릿지 수정이 조용히 무시된다.

## 구조

- `bridge/kakao.swift` — AX 조작 전부. `@_cdecl aside_request(JSON) → JSON` 단일 진입점.
  자체 구현(`AXUIElement` 익스텐션 기반, `KakaoDriver`). **라벨 매칭은 반드시 한/영
  세트로**(`Kakao.listWindowNames`, `Kakao.sendButtonNames` 등) — 로케일 무관 동작이
  핵심. AX 동작 특성(가상화·전송확인·기하학 발신자 구분)은 이 세션에서 프로브로
  직접 재검증한 사실 기반.
- `internal/kakao/` — cgo 래퍼(bridge.go, 뮤텍스로 직렬화) + 타입 커넥터(connector.go).
- `internal/ui/` — Bubble Tea **v2** TUI (import 경로는 `charm.land/bubbletea/v2` —
  github 경로로 받으면 vanity path 에러남). kakao 호출은 전부 tea.Cmd 비동기.
  v2를 쓰는 이유: 실커서 API(`tea.View.Cursor`). 한글 IME 조합 글자는 터미널
  실커서 위치에 그려지므로, 실커서를 입력줄 타이핑 지점에 둬야 preedit이 제자리에 뜬다.
  textinput의 커서 X는 룬 단위라 한글(2칸 폭)용으로 셀 폭 재계산 보정이 View()에 있음.
- `main.go` — 서브커맨드: doctor / ls / read / open / send / (없으면 TUI).

## 도메인 지식 (프로브로 직접 검증 — 지우지 말 것)

- 카톡 대화 목록 행은 **가상화**됨: 뷰포트 밖 행은 이웃 방의 stale 라벨을 노출.
  페이지 단위로 스크롤 후 읽어야 함 (conversations()의 pageSize 로직).
- **방 열기는 좌표 클릭 없이 선택+Return만 사용 (실검증):**
  행 자체엔 AX 액션이 없고 postToPid 마우스 클릭은 카톡이 무시하지만,
  ① 테이블 `AXSelectedRows`로 행 선택 ② 테이블 AXFocused ③ Return 키 postToPid
  조합이면 **카톡을 전면으로 가져오지 않고** 방이 열린다. 선택은 좌표가 아니라
  행 객체에 붙으므로 열기 도중 목록이 재정렬돼도 따라감 — 옛 더블클릭 폴백의
  "엉뚱한 방 열림" 레이스가 구조적으로 없음. 메인 창 복구(메뉴 press)도
  activate 없이 동작.
- 열기 사다리(openRow): 조용한 선택+Return ×2 → 카톡 잠깐 활성화 후 같은 방식
  1회(백그라운드에서 포커스를 안 내줄 때) → 실패 시 Go 쪽(OpenRoom)이 목록
  재스캔 후 새 행 번호로 1회 재시도. CGEvent 좌표 클릭 방식은 "엉뚱한 방 열림"
  레이스가 있어 채택하지 않음 — 부활시키지 말 것.
- Return을 쏘기 전 반드시 포커스가 테이블인지 확인(CFEqual)할 것 — 아니면
  열린 채팅 입력창에 엔터가 들어가 오발송 위험.
- 전송 확인의 유일한 근거는 **입력창이 비워지는 것**. AXPress 반환 코드는
  성공/실패 모두 거짓말할 수 있음. 절대 재-press 금지(중복 전송).
- 전송 버튼 활성화를 위해 AXValue 설정 후 space+backspace 키 이벤트로
  카톡의 text-change 핸들러를 깨워야 하는 경우가 있음.
- 내/상대 메시지 구분은 라벨이 아니라 **말풍선 좌우 기하학** (언어 무관).
- 사진·영상·이모티콘은 텍스트 영역 없는 자체 행 → readable 카운트로 건너뜀.
- **런루프 없는 프로세스(Go 호스트)에서 NSWorkspace는 못 쓴다**:
  `NSWorkspace.shared.runningApplications`/`frontmostApplication`은 런루프가
  돌아야 갱신되는 스냅샷이라 영원히 stale. 앱 조회는
  `NSRunningApplication.runningApplications(withBundleIdentifier:)`(매 호출
  라이브), 최전면 앱은 AX 시스템와이드 `AXFocusedApplication`으로. 같은 이유로
  같은 프로세스에서 읽는 `isHidden`도 stale일 수 있음 — 상태 확인은 새 조회로.
- 숨김은 `NSRunningApplication.hide()`가 아니라(여기선 false 반환하며 거부)
  **AX `kAXHiddenAttribute` 세팅**으로. 숨김 상태에서 창 생성·읽기·전송 모두
  동작하며 창을 새로 열어도 숨김이 풀리지 않음(실검증).

## 검증 상태 (2026-09-22)

- 전 경로 실검증 완료: doctor, ls, read, open(더블클릭 재열기 포함), send(TUI에서
  실전송 확인), 한글 IME 커서(실커서 방식), 창 닫힘 자동 복구(EnsureOpen 경로).
- 영어 UI 카톡에서 검증됨. 한글 UI는 라벨 세트에 포함돼 있으나 실기기 미확인.
- 구조적 제약(사용자 안내 필요): 카톡 앱은 실행 중이어야 하고(숨김은 OK),
  방을 읽으려면 그 방 창이 존재해야 함 — 없으면 무소음 경로로 자동으로 열며,
  그 방은 읽음 처리됨. 포커스는 뺏지 않음(폴백 경로일 때만 잠깐 전면).

## 알려진 한계 / 다음 후보

- 폴링(3s) 기반 갱신 → AXObserver 알림 기반으로 교체 후보.
- `삭제됨/Edited` 등 영어 시스템 문구 일부는 추정값 (실물 미확인, 오탐 무해).
- swiftc 타깃(16.0)과 go 링커(15.1) 버전 경고 — 무해, 배포 시 -target 통일.
