# Magnolia Arbitrage System 🌸

Magnolia는 국내외 암호화폐 거래소(Upbit, Coinbase 등) 간의 가격 차이(프리미엄) 및 펀딩비 차익을 실시간으로 감지하고, **100% 델타 중립(Delta-Neutral) 헷징**과 **AI 기반 프리미엄 궤적 예측**을 통해 자동 분산 주문을 수행하는 기관급 아비트라지 트레이딩 시스템입니다.

이 문서는 Magnolia 시스템의 **전체 아키텍처**, **파트별(AI · 백엔드 · 프론트엔드) 코드 수정 가이드**, **수정 후 테스트 방법**, 그리고 **서버 빌드 및 구동 매뉴얼**을 다룹니다.

---

## 📑 목차 (Table of Contents)
1. [🏗 시스템 아키텍처 및 컨테이너 구성](#-시스템-아키텍처-및-컨테이너-구성)
2. [🛠 필수 인프라 & 환경 변수](#-필수-인프라--환경-변수)
3. [🚀 전체 서버 빌드 및 띄우는 방법 (Quick Start)](#-전체-서버-빌드-및-띄우는-방법-quick-start)
4. [🧭 파트별 수정 방향 · 빌드 · 테스트 매뉴얼](#-파트별-수정-방향--빌드--테스트-매뉴얼)
   - [4.1 AI 서비스 (`ai_service/`) 수정 및 테스트](#41-ai-서비스-ai_service-수정-및-테스트)
   - [4.2 백엔드 (`backend/`) 수정 및 테스트](#42-백엔드-backend-수정-및-테스트)
   - [4.3 프론트엔드 (`frontend/`) 수정 및 테스트](#43-프론트엔드-frontend-수정-및-테스트)
5. [🧪 통합 테스트 및 검증 가이드](#-통합-테스트-및-검증-가이드)
6. [☁️ 프로덕션 & 클라우드 (K8s / AWS / GCP) 배포 가이드](#️-프로덕션--클라우드-k8s--aws--gcp-배포-가이드)

---

## 🏗 시스템 아키텍처 및 컨테이너 구성

프로젝트 루트의 [`docker-compose.yml`](file:///home/jh95kr2004/Workspace/magnolia/docker-compose.yml)을 통해 총 8개의 컨테이너가 유기적으로 동작합니다.

| 서비스명 (`container_name`) | 소스 경로 / 빌드 파일 | 포트 | 핵심 역할 |
| :--- | :--- | :--- | :--- |
| **`postgres`** (`magnolia_postgres`) | `postgres:15-alpine` | `5432` | 사용자 계정, 암호화된 API Key, 활성 포지션 상태, 포트폴리오 자산 히스토리 및 모의매매(Paper) 상태 영속화 |
| **`redis`** (`magnolia_redis`) | `redis:7-alpine` | `6379` | 실시간 시세 Pub/Sub 브로드캐스팅, AI 예측 신호 및 다중 해상도(`24H`/`7D`/`1M`/`1Y`) 프리미엄 차트 캐싱 |
| **`influxdb`** (`magnolia_influxdb`) | `influxdb:2.7-alpine` | `8086` | 밀리초 단위 실시간 틱(Ticker) 및 호가창(Orderbook) 시계열 적재 (AI 모델 학습 및 차트 백필용) |
| **`browser_session`** (`magnolia_browser_session`) | `selenium/standalone-chromium` | `6080` (noVNC)<br>`9222` (CDP) | 공식 API에 코인 대여가 없는 거래소(Upbit 등)를 위한 CDP 기반 웹 코인 대여 자동화 브라우저 및 `noVNC` 원격 로그인 뷰어 |
| **`ai`** (`magnolia_ai`) | [`ai_service/app.py`](file:///home/jh95kr2004/Workspace/magnolia/ai_service/app.py) | 내부 통신 | 실시간 시세를 구독하여 향후 8시간 프리미엄 궤적을 예측하고, 다중 해상도(`24H` 1분봉, `7D` 15분봉, `1M` 1시간봉, `1Y` 1일봉) 차트 데이터를 집계하여 Redis에 캐싱 |
| **`feed`** (`magnolia_feed`) | [`backend/cmd/feed`](file:///home/jh95kr2004/Workspace/magnolia/backend/cmd/feed)<br>([`Dockerfile.master`](file:///home/jh95kr2004/Workspace/magnolia/backend/Dockerfile.master)) | 내부 통신 | 글로벌 거래소(Upbit, Coinbase 등) WebSocket/REST 오더북 및 환율을 실시간 수집하여 Redis Pub/Sub과 InfluxDB로 전파 |
| **`engine`** (`magnolia_engine`) | [`backend/cmd/engine`](file:///home/jh95kr2004/Workspace/magnolia/backend/cmd/engine)<br>([`Dockerfile.master`](file:///home/jh95kr2004/Workspace/magnolia/backend/Dockerfile.master)) | 내부 통신 | 사용자별 독립 고루틴(`runUserEngine`)을 구동하여 실전(LIVE) 및 모의(PAPER) 아비트라지 매매, 100% 델타 중립 자동 리밸런싱, 웹 대여 세션 워치독 수행 |
| **`node`** (`magnolia_node`) | [`backend/cmd/node`](file:///home/jh95kr2004/Workspace/magnolia/backend/cmd/node) + [`frontend/`](file:///home/jh95kr2004/Workspace/magnolia/frontend)<br>([`Dockerfile.node`](file:///home/jh95kr2004/Workspace/magnolia/backend/Dockerfile.node)) | `8081` | 빌드된 React 프론트엔드 SPA 정적 서빙 및 REST API / SSE(Server-Sent Events) 실시간 스트리밍을 제공하는 무상태(Stateless) 게이트웨이 서버 |

---

## 🛠 필수 인프라 & 환경 변수

### 필수 요구사항
- **Docker & Docker Compose v2**: 전체 컨테이너 빌드 및 오케스트레이션
- **Go 1.25+**: 백엔드 로컬 빌드 및 단위 테스트 실행 시
- **Python 3.10+ (PyTorch 환경)**: `ai_service` 모델 학습 및 추론 데몬 구동 (`torch`, `numpy`, `pandas`, `redis`, `influxdb-client`, `requests`)
- **Node.js 20+**: 프론트엔드 로컬 개발 서버(`vite`) 및 테스트(`vitest`, `playwright`) 실행 시

### 주요 환경 변수
- `POSTGRES_DSN`: PostgreSQL 접속 문자열 (예: `host=postgres user=magnolia_user password=magnolia_password dbname=magnolia_db port=5432 sslmode=disable`)
- `REDIS_URL`: Redis 접속 URL (예: `redis://redis:6379/0`)
- `INFLUX_URL` / `INFLUX_TOKEN` / `INFLUX_ORG` / `INFLUX_BUCKET`: InfluxDB v2 접속 정보
- `ACTIVE_EXCHANGES`: 활성화할 거래소 목록 (예: `upbit,coinbase`)
- `TARGET_COINS`: 스캔 및 매매 대상 코인 심볼 (예: `BTC,ETH`)
- `CHROME_CDP_URL`: 웹 코인 대여 자동화용 Chromium CDP 엔드포인트 (예: `http://browser_session:9222`)
- `ENCRYPTION_MASTER_KEY`: 사용자 API Secret Key 암호화용 32바이트 AES-GCM 마스터 키

---

## 🚀 전체 서버 빌드 및 띄우는 방법 (Quick Start)

### 1. 전체 서비스 한 번에 빌드 및 기동
프로젝트 루트(`/home/jh95kr2004/Workspace/magnolia`)에서 아래 명령어를 실행하면 DB 인프라부터 AI, Feed, Engine, Node(프론트엔드 포함)까지 모두 빌드되어 백그라운드로 실행됩니다.

```bash
# 전체 컨테이너 이미지 빌드 및 백그라운드 실행
docker compose up -d --build
```

### 2. 실행 중인 컨테이너 상태 및 로그 확인
```bash
# 전체 컨테이너 상태(Up / Healthy 여부) 확인
docker compose ps

# 특정 서비스 실시간 로그 확인 (예: node, engine, feed, ai)
docker compose logs -f node
docker compose logs -f engine
docker compose logs -f ai
```

### 3. 파트별 재빌드 / 재시작 요약 치트시트
코드를 수정한 뒤 변경 사항을 서버에 즉시 반영할 때 사용하는 명령어입니다.

| 수정한 영역 | 반영 명령어 | 비고 |
| :--- | :--- | :--- |
| **AI 서비스 (`ai_service/**`)** | `docker restart magnolia_ai` | `./ai_service` 디렉토리가 컨테이너 `/app`에 볼륨 마운트되어 있어 이미지 재빌드 없이 재시작만으로 즉시 반영 |
| **프론트엔드 (`frontend/**`)** | `docker compose up -d --build node` | [`Dockerfile.node`](file:///home/jh95kr2004/Workspace/magnolia/backend/Dockerfile.node)가 `npm run build`를 수행하여 `magnolia_node` 안에 최신 SPA 번들을 내장 |
| **백엔드 API / 차트 (`backend/internal/api/**`, `cmd/node/**`)** | `docker compose up -d --build node` | 프론트엔드와 통신하는 REST API 및 SSE 게이트웨이 재빌드 |
| **백엔드 매매 엔진 / 전략 (`backend/internal/strategy/**`, `cmd/engine/**`)** | `docker compose up -d --build engine` | 코어 매매 엔진(`magnolia_master` 이미지) 재빌드 및 재기동 |
| **백엔드 시세 피드 (`backend/cmd/feed/**`, `internal/exchange/**`)** | `docker compose up -d --build feed engine node` | 거래소 인터페이스 변경 시 연관 서비스 일괄 재빌드 |

---

## 🧭 파트별 수정 방향 · 빌드 · 테스트 매뉴얼

---

### 4.1 AI 서비스 (`ai_service/`) 수정 및 테스트

#### 📂 주요 파일 구조 및 수정 포인트
- [`ai_service/app.py`](file:///home/jh95kr2004/Workspace/magnolia/ai_service/app.py):
  - **실시간 추론 및 24시간 차트 루프**: 2초 주기로 Redis의 최신 호가를 읽어 향후 8시간 프리미엄 예측(`forecast`)과 과거 24시간 1분봉 히스토리(`market:premium_chart:{baseEx}:{baseSym}:{foreignEx}:{foreignSym}`)를 갱신합니다.
  - **`LongRangeChartManager` (장기 다중 해상도 차트 집계기)**: 백그라운드 스레드에서 **`7D`(15분봉, 672포인트)**, **`1M`(1시간봉, 720포인트)**, **`1Y`(1일봉, 365포인트)** 히스토리와 과거 AI 인과적 균형 추세선(`_compute_causal_equilibrium_target`)을 주기적으로 계산하여 각각 `market:premium_chart:{7D|1M|1Y}:...` 키로 Redis에 별도 캐싱합니다.
- [`ai_service/data/influx_client.py`](file:///home/jh95kr2004/Workspace/magnolia/ai_service/data/influx_client.py):
  - InfluxDB 시계열 쿼리 및 거래소 공개 캔들 API(Upbit 분봉/일봉, Coinbase 캔들, 일별 과거 USD/KRW 환율) 백필 로직(`fetch_long_range_spreads`, `fetch_recent_spreads`)이 구현되어 있습니다. 새로운 거래소나 시간 프레임(예: `3M`, `6M`)을 추가할 때 이 파일을 수정합니다.
- [`ai_service/model/predictor.py`](file:///home/jh95kr2004/Workspace/magnolia/ai_service/model/predictor.py) & [`ai_service/model/rl_agent.py`](file:///home/jh95kr2004/Workspace/magnolia/ai_service/model/rl_agent.py):
  - 각각 프리미엄 예측 신경망(`PremiumForecaster`)과 포지션 진입/종료 강화학습 에이전트(PPO `RLAgent`) 모델 아키텍처가 정의되어 있습니다.
- [`ai_service/env/`](file:///home/jh95kr2004/Workspace/magnolia/ai_service/env):
  - 강화학습 학습용 아비트라지 시뮬레이션 환경으로, 백엔드(`backend/internal/strategy`)의 수수료·펀딩비·슬리피지 로직과 동일하게 유지해야 합니다.

#### 🧪 AI 서비스 수정 후 테스트 및 반영 방법
1. **문법 및 로컬 스크립트 검증**:
   ```bash
   # 컨테이너 내부와 동일한 Python 환경으로 문법/모듈 로드 검증
   /home/jh95kr2004/miniconda3/envs/torch_env/bin/python -m py_compile ai_service/app.py ai_service/data/influx_client.py
   ```
2. **모델 학습 및 평가 (모델 구조/파라미터 변경 시)**:
   ```bash
   cd ai_service
   # 초기 데이터셋 부트스트랩 또는 InfluxDB 데이터 추출
   /home/jh95kr2004/miniconda3/envs/torch_env/bin/python bootstrap_dataset.py
   # 프리미엄 예측기 학습 및 평가
   /home/jh95kr2004/miniconda3/envs/torch_env/bin/python train_forecaster.py
   /home/jh95kr2004/miniconda3/envs/torch_env/bin/python evaluate_forecaster.py
   # 강화학습 에이전트 학습 및 평가
   /home/jh95kr2004/miniconda3/envs/torch_env/bin/python train_agent.py
   /home/jh95kr2004/miniconda3/envs/torch_env/bin/python evaluate_agent.py
   ```
3. **컨테이너 재시작 및 Redis 캐시 검증**:
   ```bash
   # AI 컨테이너 재시작
   docker restart magnolia_ai
   docker logs --tail 50 magnolia_ai

   # Redis에 다중 해상도 차트 캐시(24H, 7D, 1M, 1Y)가 정상 생성되었는지 확인
   docker exec magnolia_redis redis-cli keys "market:premium_chart*"
   ```

---

### 4.2 백엔드 (`backend/`) 수정 및 테스트

#### 📂 주요 파일 구조 및 수정 포인트
- [`backend/internal/api/`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/api):
  - [`handlers_ws.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/api/handlers_ws.go): 대시보드 및 글로벌 기회 탐색용 SSE 스트리밍, `/api/opportunities/chart` (`range=1H|6H|24H|7D|1M|1Y` 다중 해상도 조회 및 모의매매 S-curve 오프셋 합성), `/api/portfolio/history` 핸들러가 위치합니다.
  - [`server.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/api/server.go): REST 라우팅, 인증 미들웨어, 모의매매 리셋/시뮬레이션 제어 API, 웹 대여 세션 제어 API를 관리합니다.
- [`backend/internal/strategy/`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy):
  - 5대 아비트라지 매매 전략([`strategy_margin_spot.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_margin_spot.go), [`strategy_margin_future.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_margin_future.go), [`strategy_foreign_transfer.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_foreign_transfer.go), [`strategy_future_spot.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_future_spot.go), [`strategy_future_future.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_future_future.go)) 및 자본 쏠림 방지를 위한 **100% 델타 중립 자동 역전송 리밸런싱** 로직이 구현되어 있습니다.
- [`backend/internal/exchange/`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/exchange):
  - 거래소 공통 인터페이스, 모의매매용 가상 거래소 클라이언트(`paper_client.go`), 그리고 CDP 기반 업비트 웹 코인 대여 드라이버(`web_lending.go`, `upbit_lending.go`)가 위치합니다.
- [`backend/internal/db/redis/pubsub.go`](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/db/redis/pubsub.go):
  - Redis Pub/Sub 시세 수신 및 `GetPremiumChartByRange` 등 캐시 조회 레이어입니다.

#### 🧪 백엔드 수정 후 테스트 및 빌드 방법
1. **Go 컴파일 및 단위 테스트 실행**:
   ```bash
   cd backend
   # 전체 패키지 컴파일 검증
   go build ./...

   # 전략, 실행, 거래소, API 단위 테스트 실행
   go test ./... -v
   ```
   *(로컬 호스트에 Go가 설치되어 있지 않은 경우 Docker 빌더를 통해 검증할 수 있습니다: `docker compose build node engine`)*
2. **수정 대상에 따른 컨테이너 빌드 및 재기동**:
   ```bash
   # 1) API 핸들러(internal/api) 또는 cmd/node 수정 시
   docker compose up -d --build node

   # 2) 매매 전략(internal/strategy) 또는 cmd/engine 수정 시
   docker compose up -d --build engine

   # 3) 시세 피드(cmd/feed) 수정 시
   docker compose up -d --build feed
   ```

---

### 4.3 프론트엔드 (`frontend/`) 수정 및 테스트

#### 📂 주요 파일 구조 및 수정 포인트
- [`frontend/src/pages/Dashboard.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/pages/Dashboard.jsx):
  - 총 평가자산 카드, 거래소 자본 비중 도넛 차트, 활성 아비트라지 포지션 테이블, 모의매매 프리미엄 시뮬레이터 컨트롤러 등을 렌더링하는 메인 대시보드입니다.
- [`frontend/src/components/dashboard/EquityChart.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/components/dashboard/EquityChart.jsx):
  - 대시보드 평가자산 추이 그래프입니다.
  - 상단 **총 평가자산** 금액과 수익률은 그래프 호버/터치나 줌 상태와 무관하게 **항상 현재 실시간 자산**을 기준으로 고정 표시되며, 특정 시점의 자산은 그래프 내부 우측 상단 툴팁에 표시됩니다.
  - 데스크톱 마우스 휠 확대/축소 및 드래그 이동, 모바일 두 손가락 핀치 인/아웃(`touchAction: 'none'` + 1손가락 세로 스크롤 전달 + 1손가락 가로 툴팁 스크러빙), 하단 시간 프레임(`1H`, `24H`, `1W`, `1M`, `1Y`, `ALL`) 및 `+`/`−` 버튼을 지원합니다.
- [`frontend/src/pages/GlobalOpportunities.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/pages/GlobalOpportunities.jsx):
  - 실시간 거래소 간 프리미엄 목록 페이지입니다.
  - **데스크톱(`> 640px`)**에서는 전체 컬럼 테이블로 표시되고, **모바일(`<= 640px`)**에서는 가로 스크롤 없이 한눈에 들어오는 **2단 그리드 카드 레이아웃**으로 자동 전환됩니다.
- [`frontend/src/components/opportunities/PremiumTrajectoryChart.jsx`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/components/opportunities/PremiumTrajectoryChart.jsx):
  - 실제 과거 프리미엄 추이 + 과거 AI 예측 궤적 + 향후 8시간 AI 예측 궤적을 그리는 인터랙티브 SVG 차트입니다.
  - **`1H`, `6H`, `24H`, `7D`, `1M`, `1Y` 프리셋 버튼**과 **자동 LOD(Level-of-Detail) 해상도 전환**이 구현되어 있어, 사용자가 핀치 아웃이나 마우스 휠로 축소하면 보이는 시간 구간(`viewWindow.start`)에 맞춰 1분봉(`24H`) ↔ 15분봉(`7D`) ↔ 1시간봉(`1M`) ↔ 1일봉(`1Y`) 데이터를 백그라운드 프리패칭과 함께 끊김 없이 전환합니다.
- [`frontend/src/styles/index.css`](file:///home/jh95kr2004/Workspace/magnolia/frontend/src/styles/index.css):
  - 다크 테마 디자인 토큰, 테이블/카드 스타일, 모바일 반응형 미디어 쿼리(`@media (max-width: 768px)`, `@media (max-width: 640px)`)가 정의되어 있습니다.

#### 🧪 프론트엔드 수정 후 테스트 및 서버 반영 방법
1. **로컬 개발 서버 구동 (선택)**:
   ```bash
   cd frontend
   npm install
   npm run dev
   ```
2. **프론트엔드 단위 테스트 및 E2E 테스트**:
   ```bash
   cd frontend
   # Vitest 컴포넌트 단위 테스트
   npm test

   # 프로덕션 번들 빌드 검증 (문법 및 번들링 에러 확인)
   npm run build

   # Playwright 브라우저 E2E 테스트
   npm run test:e2e
   ```
3. **운영 서버(`magnolia_node`)에 프론트엔드 빌드 및 배포**:
   프론트엔드 정적 파일은 `magnolia_node` 컨테이너 빌드 시 [`backend/Dockerfile.node`](file:///home/jh95kr2004/Workspace/magnolia/backend/Dockerfile.node)의 `frontend-builder` 스테이지에서 자동으로 `npm run build`를 거쳐 `/app/frontend/dist`에 탑재됩니다. 따라서 프론트엔드 코드를 수정한 뒤에는 아래 명령어 한 줄로 빌드와 배포가 완료됩니다.
   ```bash
   # 프로젝트 루트에서 실행
   docker compose up -d --build node
   ```

---

## 🧪 통합 테스트 및 검증 가이드

코드 수정 및 컨테이너 재기동 후 시스템이 정상 동작하는지 점검하는 체크리스트입니다.

### 1. Redis 시세 및 다중 해상도 차트 캐시 점검
```bash
# 1) 모든 차트 해상도(24H, 7D, 1M, 1Y) 키 존재 여부 확인
docker exec magnolia_redis redis-cli keys "market:premium_chart*"

# 2) 각 해상도별 포인트 개수 및 분봉 간격(step_min) 검증
for k in \
  "market:premium_chart:UPBIT:BTC-KRW:COINBASE:BTC-USD" \
  "market:premium_chart:7D:UPBIT:BTC-KRW:COINBASE:BTC-USD" \
  "market:premium_chart:1M:UPBIT:BTC-KRW:COINBASE:BTC-USD" \
  "market:premium_chart:1Y:UPBIT:BTC-KRW:COINBASE:BTC-USD"; do
  echo "=== $k ==="
  docker exec magnolia_redis redis-cli get "$k" | python3 -c "import sys, json; d=json.load(sys.stdin); h=d.get('history',[]); print('step_min:', d.get('step_min'), 'history_len:', len(h), 'first_offset:', h[0].get('offset_min') if h else None, 'last_offset:', h[-1].get('offset_min') if h else None)"
done
```
- 정상 출력 기준:
  - 기본(`24H`): `step_min: 1`, `history_len: 1440`
  - `7D`: `step_min: 15`, `history_len: 672`
  - `1M`: `step_min: 60`, `history_len: 720`
  - `1Y`: `step_min: 1440`, `history_len: 365`

### 2. 로컬 E2E 샌드박스 시뮬레이션 테스트
실제 자본 투입 없이 `[Feed Handler]` ↔ `[AI Daemon]` ↔ `[Engine]` 간 파이프라인을 로컬에서 빠르게 검증하려면 샌드박스 스크립트를 실행합니다.
```bash
cd backend
go run ./cmd/sandbox/main.go
```

---

## ☁️ 프로덕션 & 클라우드 (K8s / AWS / GCP) 배포 가이드

### 1. Docker 이미지 빌드 및 레지스트리 푸시
```bash
# Master 이미지 (engine + feed 통합 바이너리) 빌드
docker build -t magnolia_master:latest -f backend/Dockerfile.master .

# Node 이미지 (React SPA + Go API/SSE 게이트웨이 통합) 빌드
docker build -t magnolia_node:latest -f backend/Dockerfile.node .
```

### 2. Kubernetes 배포 매니페스트 ([`k8s/`](file:///home/jh95kr2004/Workspace/magnolia/k8s))
- [`k8s/master-deployment.yaml`](file:///home/jh95kr2004/Workspace/magnolia/k8s/master-deployment.yaml):
  - 사용자별 고루틴을 관리하는 Stateful 코어 매매 엔진(`cmd/engine`) 배포 설정입니다.
  - 롤링 업데이트 시 기존 관리 중이던 사용자를 Redis Dangling Client Queue로 안전하게 이관하고, 장애 복구 시 DB에 기록된 `NODE_ID`를 기반으로 즉시 포지션 관리를 복원합니다.
- [`k8s/node-deployment.yaml`](file:///home/jh95kr2004/Workspace/magnolia/k8s/node-deployment.yaml):
  - 프론트엔드 웹 트래픽과 SSE 스트리밍을 처리하는 Stateless 인터페이스 서버(`cmd/node`) 배포 설정으로, 트래픽에 따라 자유롭게 스케일 아웃(Scale-out)이 가능합니다.
