# Phase 1. 파일 구조 파악

[x] Task 1.1 - GEMINI.md와 README.md를 분석하여 현재까지 작성된 프로젝트의 현황 이해
[x] Task 1.2 - 파일 구조에 대한 이해와 각 파일의 기능 파악 후 GEMINI.md 업데이트

# Phase 2. 서버 구조 파악

[x] Task 2.1 - backend 디렉토리 파일을 분석하여 서버 구동 방식 파악. 특히 각 거래소 간의 실시간 데이터를 가져오는 부분, 가져온 데이터를 이용해 아비트라지 매매 기회를 파악하는 부분, 각 사용자 별로 발견된 매매 기회를 통해 새 포지션을 잡거나 기존의 포지션을 추가 진입 혹은 (부분) 청산하는 부분에 대해 이해.
[x] Task 2.2 - Task 2.1에서 파악한 내용을 기반으로 DB에 데이터를 저장하거나 AI 엔진과 소통하는 방식을 이해.
[x] Task 2.3 - Task 2.1에서 파악한 내용을 기반으로 Docker 위에서 서버를 어떻게 실행하고 배포 및 운영하는 지 이해.
[x] Task 2.4 - Task 2.1~2.3에서 파악한 내용을 기반으로 서버 구조 파악 문서 작성. 특히 AI 엔진과의 인터페이스를 중심으로 서술. AI 엔진에게 넘겨주는 데이터와 엔진으로 부터 기대하는 응답을 서술하고 그 응답을 이용해 어떻게 매매 기회를 포착하고 사용자 별로 포지션을 잡는지 서술.

# Phase 3. AI 엔진 이해

[x] Task 3.1 - AI 엔진이 동작하는 방식을 이해. 특히 거래소 별로 앞으로의 가격 프리미엄을 예측하는 forecaster와 예측된 프리미엄 가격을 기반으로 매매 기회를 예측하는 agent모델을 구분하여 이해.
[x] Task 3.2 - Phase 2에서 이해한 서버 구조에 맞게 AI 엔진이 backend 서버와 어떻게 소통하는 지를 이해.
[x] Task 3.3 - 현재 backend 서버와 AI 엔진 사이에 미개발 부분 혹은 잘못 구현된 부분이 있는지 확인.
  - [x] **AI 추론기([app.py](file:///home/jh95kr2004/Workspace/magnolia/ai_service/app.py))의 `state_vector` 차원 수정 (17차원 -> 18차원 -> 22차원 상태머신)**
    - [arbitrage_env.py](file:///home/jh95kr2004/Workspace/magnolia/ai_service/env/arbitrage_env.py)에 맞춰 18번째 피처 `explicit_friction` 및 4개 상태머신 One-hot 피처 추가
  - [x] **AI 모델의 `action_score`와 백엔드 임계값(`MinTargetProfit`) 연동 논리 수정**
  - [x] **AI 추론 입력(`state_vector`) 내 계좌 상태 변수(`exposure`, `balance`, `unrealized_pnl` 등) 동적 피딩 연동 검토**
  - [x] **백엔드 매매 집행 시 AI 모델의 롱/숏 방향성(Long/Short) 판단 반영 설계**
  - [x] **백엔드 엔진 내 AI 포지션 축소(Scale-out) 및 전체 청산(Exit) 실행 로직 구현**
  - [x] **AI 모델의 최적 경로 비용 계산과 백엔드 전략 객체(A~E) 간의 라우팅 매핑 구현**
[x] Task 3.4 - Phase 3에서 파악한 내용을 기반으로 AI 엔진 구조 파악 문서 작성.

# Phase 4. 파일 구조 정리

[x] Task 4.1 - Phase 2와 3을 통해 이해한 내용을 기반으로 잘못된 파일 구조나 코드를 파악하고 수정안을 제시. AI엔진과 backend 서버 사이에 잘못된 인터페이스 수정 뿐만 아니라 사용하지 않는 trash 파일들도 파악 후 삭제를 제안.
[x] Task 4.2 - 수정안을 바탕으로 코드 수정.

# Phase 5. 테스트

[x] Task 5.1 - AI 엔진과 backend 서버가 서로 실시간 데이터를 받아와 알맞은 아비트라지 매매를 수행하는 (얼마나 많은 수익을 내는지 확인하는) 테스트 방법을 제시
[x] Task 5.2 - 제시된 테스트를 바탕으로 현재의 AI 엔진과 backend 서버의 구조가 아비트라지 매매를 하기에 적합한 구조를 갖고 있는지 파악.

# Phase 6. 아비트라지 엔진 고도화

[x] Task 6.1 - Phase 5에서 파악한 내용을 바탕으로 AI 엔진 및 backend 서버 개선안 제시.
[x] Task 6.2 - 개선안을 바탕으로 코드 수정.
  - [x] **1단계: [arbitrage_env.py](file:///home/jh95kr2004/Workspace/magnolia/ai_service/env/arbitrage_env.py) 고도화**
    - [x] 전송 루프(Transfer Loop) 상태 머신 및 네트워크 지연 시간 시뮬레이션 구현
    - [x] 비선형 슬리피지 모델(Orderbook Depth 연동) 및 가변 대여 이자율(Borrow Rate) 반영
    - [x] Action Space(Dict형: exposure, action_type) 및 Observation Space 재정의
  - [x] **2단계: 다중 자산(Multi-pair) 포트폴리오 에이전트 훈련 스크립트 작성**
    - [x] [train_agent.py](file:///home/jh95kr2004/Workspace/magnolia/ai_service/train_agent.py) 다자산 병렬 훈련 스키마로 업데이트 및 훈련 집행
  - [x] **3단계: 실시간 계좌 정보 피드백 연동 및 추론기 리팩토링**
    - [x] [app.py](file:///home/jh95kr2004/Workspace/magnolia/ai_service/app.py) 수정: Redis를 통해 백엔드에서 사용자 계좌 상태(Balance, Exposure 등)를 동적으로 읽어 state_vector 구성
    - [x] [app.py](file:///home/jh95kr2004/Workspace/magnolia/ai_service/app.py)의 예측 결과 출력에 롱/숏 방향성 및 action_type(Trade, Transfer, Repay) 명시하여 퍼블리시
    - [x] **[README.md](file:///home/jh95kr2004/Workspace/magnolia/ai_service/README.md) 고도화**: 모의 매매(Sandbox) 및 실시간 연동을 위한 데이터 추출(extract) 및 실행 커맨드 가이드 상세 작성
  - [x] **4단계: 백엔드 Go 엔진 연동 및 동적 전략 라우터 구현**
    - [x] [pubsub.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/db/redis/pubsub.go) 및 [engine.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/engine.go)에서 계좌 상태 전송 및 양방향 포지션/청산/라우팅 로직 실행 코드 반영
[x] Task 6.3 - 수정안을 바탕으로 테스트 수행 및 아비트라지 엔진의 성능 (얼마나 많은 수익을 내는지) 확인 (pytest 환경 및 sandbox E2E 테스트 검증 완료).

# Phase 7. 추가 최적화 및 개선 (Post-Migration Optimization)

[x] Task 7.1 - **Strategy C (Transfer Loop)의 마진 숏-현물 롱 포지션 즉시 상환 자동화**
  - **이유**: 현물 헷징 시 코인 전송이 완료되면 가격 차이가 완전히 락킹(Delta-neutral)되므로 대기 시간 동안 발생하는 불필요한 마진 대여 이자를 줄이기 위해 AI의 REPAY 신호를 기다리지 않고 백엔드에서 즉시 상환(Repay)을 집행하도록 최적화.
  - **내용**: [engine.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/engine.go) 또는 [strategy_foreign_transfer.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_foreign_transfer.go)에서 전송 완료 감지 시 자동으로 상환 프로세스를 호출하고 포지션을 CLOSE 하도록 변경.

# Phase 8. 실전 아비트라지 고도화 및 단일 Monorepo Git 재구축

[x] Task 8.1 - **전략 D ([strategy_future_spot.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_future_spot.go)) 및 전략 E ([strategy_future_future.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_future_future.go)) 기대수익률 수식 부호 버그 수정** 및 전 전략 `ExecuteScale` 구현
[x] Task 8.2 - **이중 조건(Dual-Trigger) 100% 델타 중립 자본 리밸런싱([strategy_foreign_transfer.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/strategy/strategy_foreign_transfer.go)) 구현** (해외 USD 비중 <= 20% & 역프/손익분기 이하 시 시작 -> 50:50 회복 시 종료)
[x] Task 8.3 - **확장형 `WebLendingDriver` ([web_lending.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/exchange/web_lending.go)) 및 `UpbitLendingDriver` ([upbit_lending.go](file:///home/jh95kr2004/Workspace/magnolia/backend/internal/exchange/upbit_lending.go)) + `noVNC` 세션 워치독 구현** (사용자별 고루틴 내 세션 감시, 세션 단절 시 대여 전략 차단 및 대여 포지션 보유 중 단절 시 CRITICAL 긴급 경보 발송)
[x] Task 8.4 - **0-Base 단일 Monorepo 마이크로 Git 커밋 히스토리 재구축**
