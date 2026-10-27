# AI Service Execution and Data Integration Guide

본 문서는 Magnolia 아비트라지 AI 서비스의 데이터 수집, 모델 학습 및 실시간 샌드박스 모의 매매 연동 방법에 대해 설명합니다.

---

## 1. 가상환경 및 의존성 설정
AI 서비스의 모든 파이썬 스크립트는 로컬 가상환경(`venv`) 하에서 구동됩니다.
```bash
# 가상환경 활성화 (필요 시)
source venv/bin/activate
pip install -r requirements.txt
```

---

## 2. 과거 시세 데이터 추출 및 수집 (Data Collection)
강화학습 및 프리미엄 예측 모델 학습에 필요한 시계열 데이터를 수집하고 추출하는 방법입니다.

### 2.1 초기 데이터 부트스트랩 (Cold Start)
서버 InfluxDB에 아직 저장된 데이터가 없는 상태에서 가격 데이터를 최초로 구축할 때 사용합니다.
```bash
# 외부 API를 호출하여 초기 학습 데이터를 로컬 data/ 폴더 하위에 캐싱
python bootstrap_dataset.py
```

### 2.2 실시간 데이터베이스(InfluxDB) 벌크 추출
가동 중인 서비스 데이터베이스에서 실시간으로 수집된 시세를 CSV 포맷으로 덤프해 학습용 파일로 추출합니다.
```bash
# 환경변수(INFLUX_URL, INFLUX_TOKEN 등)가 설정된 상태에서 수행
python extract_data.py
```
* 추출 결과물은 `data/historical_dump.csv` 파일로 적재되며, 훈련 스크립트가 이를 자동으로 읽어 가동합니다.

---

## 3. 가격 예측 및 심층 강화학습(DRL) 에이전트 모델 학습 (Training)
모델 학습은 시세 변동 예측 모델 학습(1단계)과 포트폴리오 에이전트 정책 학습(2단계) 순서로 순차적으로 이루어집니다.

### 3.1 [1단계] 프리미엄 및 펀딩비 변동 예측 모델 학습
```bash
# 미래 5시간 가격 프리미엄 및 펀딩비 궤적을 예측하는 Attention-GRU 모델 훈련
python train_forecaster.py
```
* **동작 원리**: `data/historical_dump.csv` 데이터를 읽고 시계열 스케일링을 수행한 뒤, `model/weights/premium_forecaster_best.pth`, `feature_scaler.pkl`, `target_scaler.pkl` 파일을 디스크에 저장합니다.

### 3.2 [2단계] 포트폴리오 매니저 DRL 에이전트 학습
```bash
# train_agent.py를 가동하여 모델 훈련 집행
python train_agent.py
```
* **동작 원리**:
  1. `data/historical_dump.csv` 데이터를 로드합니다.
  2. 1단계에서 학습한 예측 모델(`PremiumForecaster`)을 사용하여 전체 데이터 세트의 미래 5시간 궤적을 precompute하여 offline 피처로 주입합니다.
  3. `ArbitrageEnv` 체계 안에서 가변 이자율 및 비선형 슬리피지 조건 하에 PPO 정책 신경망을 훈련합니다.
  4. 최종 훈련 가중치는 `model/weights/ppo_arbitrage_final.zip` 및 `vec_normalize.pkl`로 자동 저장됩니다.

---

## 4. 실시간 샌드박스 모의 매매 및 데몬 연동 (Sandbox Execution)
학습 완료된 DRL 모델 가중치를 기반으로 백엔드 Go 엔진과 양방향 Closed-Loop로 모의 투자를 가동합니다.

### 4.1 AI 추론 데몬 실행
```bash
# Redis PubSub 채널을 통한 실시간 추론 데몬 기동
python app.py
```
* **인터페이스 스키마**:
  * **수신(Subscribe)**: `"broadcast:opportunities:raw"` 채널로부터 시장 시세 및 사용자 실제 자산 상태(`available_balance`, `current_exposure`, `unrealized_pnl`, `state`)를 동적으로 받습니다.
  * **송신(Publish)**: 예측이 완결되면 백엔드로 `"broadcast:opportunities:ai_predicted"` 채널을 통해 타겟 비중(`target_weight`), 연동 방향성(`action_score`), 그리고 매매 실행 타입(`action_type` - 0: TRADE, 1: TRANSFER, 2: REPAY)을 전송합니다.

### 4.2 백엔드 샌드박스 및 E2E Mock 실행
AI 추론 데몬이 켜진 상태에서 백엔드 샌드박스 매매 모듈을 가동하여 실시간 연동을 검증합니다.
```bash
# 백엔드 루트 폴더에서 redis 및 pg 인프라가 켜진 상태로 가동
cd ../backend
go run cmd/sandbox/main.go
```
* 샌드박스 엔진이 AI의 결정을 반영하여 매매를 체결하고, 체결 결과와 지갑 상태 피드백을 다시 Redis를 통해 파이썬 AI 데몬으로 무한 루프로 중계합니다.
