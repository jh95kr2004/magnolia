# 📏 Magnolia Project Development & Operational Rules (`RULES.md`)

이 문서는 Magnolia 아비트라지 시스템 개발 및 운영 과정에서 수립된 **핵심 작업 규칙(Rules)과 설계 원칙**을 정리한 문서입니다. 모든 코드 수정, AI 모델 개선, UI/UX 개편 및 배포 작업 시 본 규칙을 반드시 준수해야 합니다.

---

## 1. Git 커밋 및 형상 관리 규칙 (Git Commit Rules)

1. **최대한 작고 원자적인(Atomic) 커밋 단위로 분할**
   - 작업 완료 후 반드시 `git commit`을 생성하되, 여러 기능이나 여러 계층의 변경 사항을 하나의 거대한 커밋으로 묶지 않습니다.
   - **각 커밋이 단일 목적·단일 논리적 변경만을 담도록 최대한 잘게 쪼개어** 생성함으로써, 커밋별 코드 리뷰와 문제 발생 시 원인 추적 및 롤백이 쉽도록 유지합니다.
2. **명확한 커밋 메시지 작성**
   - 변경 대상 컴포넌트(`ai`, `backend`, `frontend`, `engine`, `paper`, `docs` 등)와 변경 의도가 한눈에 드러나도록 작성합니다.

---

## 2. 빌드 · 서버 반영 · 테스트 검증 규칙 (Build, Deploy & Test Rules)

1. **코드 수정 후 구동 중인 서버에 즉시 빌드·반영 확인**
   - 로컬 소스 코드만 수정하고 끝내지 않으며, 수정된 파트에 해당하는 컨테이너(`magnolia_node`, `magnolia_engine`, `magnolia_feed`, `magnolia_ai`)를 **반드시 재빌드 및 재기동**하여 실제 서비스 환경에서 즉시 동작을 검증합니다.
2. **AI 모델 수정 시 성능 선(先) 평가 후 서버 반영**
   - 프리미엄 예측 모델(`PremiumForecaster`)이나 포지션 결정 강화학습 모델(`RLAgent`)의 아키텍처·피처·하이퍼파라미터를 변경할 때는 **먼저 실데이터 및 평가 스크립트(`evaluate_forecaster.py`, `evaluate_agent.py`)를 통해 예측력·수익률이 실제로 개선되는지 검증한 뒤** 운영 서버에 적용합니다.
3. **원라인(One-Line) 통합 제어 및 자동화 테스트 유지**
   - 서버의 빌드·기동·종료·SSL 설정·상태 확인·테스트는 프로젝트 루트의 [`magnolia.sh`](file:///home/jh95kr2004/Workspace/magnolia/magnolia.sh) 스크립트 한 줄로 수행할 수 있어야 합니다.
   - 모든 컴포넌트(AI, 백엔드, 프론트엔드, 헤드리스 브라우저 E2E)에 대한 단위 테스트 및 E2E 테스트(`./magnolia.sh test [all|ai|backend|frontend|e2e]`)가 항상 통과 상태를 유지해야 합니다.
4. **도메인 및 환경 설정값 하드코딩 금지**
   - 접속 도메인(예: `magnolia.hooneken.com`) 등을 설정 파일에 고정 하드코딩하지 않고, `./magnolia.sh start [domain]`과 같이 실행 시 인자나 환경 변수로 받아 [`nginx.conf`](file:///home/jh95kr2004/Workspace/magnolia/nginx.conf)를 동적으로 생성·적용합니다.

---

## 3. 아키텍처 · 상태 격리 · 백엔드 원칙 (Architecture & Isolation Rules)

1. **사용자별 완벽한 상태 격리 (Per-User State Isolation)**
   - 실전 매매 고루틴(`runUserEngine`)뿐만 아니라 **모의매매(Paper Trading) 시뮬레이션 오프셋(`SimState`), 가상 잔고, 활성 포지션 상태는 반드시 사용자(`user_id`)별로 독립 격리**되어야 합니다.
   - 특정 사용자가 프리미엄 시뮬레이션을 실행하거나 설정을 변경하더라도 다른 사용자의 화면이나 매매 엔진에 절대 영향을 주지 않아야 합니다.
2. **다중 거래소 및 다중 통화 확장성 보장**
   - 대시보드, 마켓 목록, 자산 비중, 통화 표시(USD / KRW 등)는 특정 2개 거래소(Upbit, Coinbase)나 단일 통화에 고정되지 않도록 설계하며, 향후 거래소나 법정통화가 추가될 때 코드 구조를 뜯어고치지 않고 확장할 수 있어야 합니다.
3. **모의매매(Paper) ↔ 실전매매(Live) 전환 및 초기화 안전장치**
   - 모의매매 역시 거래소의 실제 실시간 호가 및 오더북 스트림을 기반으로 동작해야 합니다.
   - 모드 전환 시 기존 포지션 일괄 정리나 모의자산 시드 초기화 같은 파괴적(Destructive) 동작은 **반드시 사용자 확인 모달(Confirm Modal)을 거쳐 승인받은 후** 실행합니다.
   - 모의자산 시드 초기화 시 대시보드의 총 평가자산 및 자산 히스토리 스냅샷도 즉시 함께 초기화되어 DB와 화면 간 괴리가 없어야 합니다.
   - 종료(청산)된 포지션은 확정 손익(`realized_pnl`)과 종료 시각(`closed_at`)을 정확히 기록하고, 대시보드의 '활성 포지션' 목록에서는 즉시 제거되어야 합니다.
4. **자가 치유(Self-Healing) 및 다중 해상도 Redis 캐싱을 통한 트래픽 최적화**
   - 거래소 WebSocket 연결 단절 시 자동 재연결 워치독(Watchdog)과 REST 캔들 백필을 통해 데이터 공백(일자 그래프 현상)을 자동으로 복구합니다.
   - 클라이언트가 차트를 조회할 때마다 거래소 API를 호출하거나 AI 추론을 다시 돌리지 않고, AI 데몬이 **분봉 해상도별(`24H`: 1분봉, `7D`: 15분봉, `1M`: 1시간봉, `1Y`: 1일봉)로 사전 집계하여 Redis에 별도 캐싱**한 데이터를 반환합니다.

---

## 4. 프론트엔드 UI/UX 및 차트 설계 규칙 (UI/UX & Chart Rules)

1. **간결하고 직관적인 인터페이스 & 전체 화면 너비 활용**
   - 불필요한 중첩 박스와 오밀조밀하게 뭉친 텍스트를 지양하고, 데스크톱에서는 좌우 여백 낭비 없이 전체 뷰포트 너비를 활용합니다.
   - 데스크톱에서는 좌측 사이드바 메뉴를, 모바일에서는 화면을 가리지 않는 1줄 컴팩트 헤더 + 드롭다운/브레드크럼 메뉴를 사용하여 상단 메뉴가 넘치거나(Overflow) 글자가 세로로 찌그러지지 않게 합니다.
2. **모바일 화면 맞춤 (Mobile-First Responsiveness)**
   - 프리미엄 목록 등 컬럼이 많은 테이블은 모바일 뷰포트(`<= 640px~768px`)에서 우측으로 잘리거나 가로 스크롤이 강제되지 않도록 **모바일 전용 카드 그리드 레이아웃**으로 자동 전환합니다.
   - 행 클릭 시 펼쳐지는 차트 역시 컨테이너 실제 폭을 측정하여 모바일 화면 너비 안에 정확히 맞춥니다.
3. **차트 확대/축소(Zoom) · 팬(Pan) 및 컨트롤 바 레이아웃 고정**
   - 프리미엄 차트([`PremiumTrajectoryChart.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/components/opportunities/PremiumTrajectoryChart.jsx))와 평가자산 차트([`EquityChart.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/components/dashboard/EquityChart.jsx)) 모두 **데스크톱 마우스 휠 확대/축소 + 드래그 이동**과 **모바일 두 손가락 핀치 인/아웃 확대/축소(`touchAction: 'none'` + 1손가락 세로 페이지 스크롤 전달)**를 지원해야 합니다.
   - 줌인/줌아웃 상태에 따라 버튼이 동적으로 생겨나며 버튼 줄이 두 줄로 밀려나는 현상(Layout Shift)이 발생하지 않도록 컨트롤 바는 항상 한 줄(`flexWrap: 'nowrap'`)을 유지합니다.
   - SVG `preserveAspectRatio="none"` 환경에서도 호버 포인트 원(Dot)이 타원형으로 찌그러지지 않도록 HTML 오버레이 등으로 정원(Circle) 비율을 유지합니다.
4. **차트 데이터 신뢰성 및 축(Axis) 표시 기준**
   - **가짜 더미 파형(Sine-wave 등) 표시 금지**: 차트 데이터가 아직 로드되지 않았을 때 임의의 가짜 그래프를 그리지 않고, 반드시 **로딩 스피너(`로딩 중...`)**를 표시합니다.
   - **실시간 테이블 행(Row) 순서 고정**: 실시간 시세 갱신으로 인해 테이블 행 순서가 계속 뒤바뀌어 사용자가 열어둔 코인 차트가 다른 코인 차트로 바뀌는 현상이 없도록 결정론적 정렬 순서를 유지합니다.
   - **대시보드 평가자산 차트([`EquityChart.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/components/dashboard/EquityChart.jsx))**:
     - **상단 '총 평가자산' 금액과 수익률은 사용자가 그래프의 다른 과거 시점을 선택(호버/터치)하거나 줌을 변경하더라도 항상 '현재 실시간 자산'을 기준으로 고정 표시**합니다. (선택한 과거 시점의 자산은 그래프 내부 툴팁에만 표시)
     - Y축 최하단은 항상 `0`(`$0.00` / `₩0`)에 고정합니다.
   - **프리미엄 궤적 차트([`PremiumTrajectoryChart.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/components/opportunities/PremiumTrajectoryChart.jsx))**:
     - Y축 `0.00%` 기준선이 차트 세로 중앙에 오도록 대칭 스케일링합니다.
     - 과거 AI 예측선은 단순 시간 지연(Lag shift)이나 1시간 단위 계단식 끊김이 없도록, AI가 실제 매매 시 참조하는 **연속 인과적 균형 추세 궤적**으로 표시하며 향후 8시간 예측 파형과 매끄럽게 이어지도록 합니다.
     - `1H`, `6H`, `24H`, `7D`, `1M`, `1Y` 구간 선택 및 줌아웃 범위에 따라 프론트엔드가 알맞은 분봉 해상도 데이터를 자동으로 전환(Auto-LOD)하여 표시합니다.

---

## 5. 문서 최신화 규칙 (Documentation Rules)

- 시스템 구조, 매매 전략, 빌드/테스트/배포 절차, 또는 운영 스크립트([`magnolia.sh`](file:///home/jh95kr2004/Workspace/magnolia/magnolia.sh))가 변경되면 [`GEMINI.md`](file:///home/jh95kr2004/Workspace/magnolia/GEMINI.md), [`README.md`](file:///home/jh95kr2004/Workspace/magnolia/README.md), [`RULES.md`](file:///home/jh95kr2004/Workspace/magnolia/RULES.md)를 함께 업데이트하여 문서와 실제 코드 간 불일치가 발생하지 않도록 유지합니다.
