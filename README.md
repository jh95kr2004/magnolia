# Magnolia Arbitrage System 🌸

Magnolia는 한국과 해외 암호화폐 거래소(Upbit, Coinbase 등) 간의 가격 차이(Kimchi Premium) 및 펀딩비 차익을 실시간으로 감지하고 자동으로 분산 주문을 수행하는 기관급 아비트라지 트레이딩 엔진입니다.

이 문서는 Magnolia 시스템을 **베어메탈(자체 서버)** 환경과 **클라우드(AWS/GCP)** 환경에서 어떻게 구축하고 배포하는지에 대한 가이드입니다.

---

## 📑 목차 (Table of Contents)
1. [🛠 필수 인프라 & 시스템 요구사항](#-필수-인프라--시스템-요구사항-prerequisites)
2. [🔑 환경 변수](#-환경-변수-environment-variables)
3. [🧪 테스트 (Test)](#-테스트-test)
    - [1. 로컬 샌드박스 통합 테스트](#1-로컬-샌드박스-통합-테스트-local-e2e-sandbox)
4. [🚀 배포 (Deploy)](#-배포-deploy)
    - [1. 베어메탈(자체 서버) 프로덕션 구동](#1-베어메탈자체-서버-프로덕션-구동)
    - [2. 클라우드 (AWS / GCP) 배포 가이드](#2-클라우드-aws--gcp-배포-가이드)

---

## 🛠 필수 인프라 & 시스템 요구사항 (Prerequisites)
- **Go 1.21+**: 백엔드 컴파일 환경
- **PostgreSQL 15+**: 전역 상태 비동기 동기화 (`LISTEN/NOTIFY` PubSub), 유저 설정, 포지션 내역 저장
- **Redis 7+**: 초고속 내부 시세 방송(Pub/Sub) 파이프라인
- **InfluxDB v2**: 밀리초 단위 틱(Tick) 및 오더북 데이터 적재 (딥러닝 AI용)
- **Node.js**: 프론트엔드 웹 UI 빌드용 (선택)

---

## 🔑 환경 변수 (Environment Variables)
서버를 실행하기 전에 설정해야 하는 주요 환경 변수입니다.

- `POSTGRES_DSN`: PostgreSQL 접속 DSN (예: `host=localhost user=postgres password=postgres dbname=magnolia_test port=5432 sslmode=disable`)
- `REDIS_URL`: Redis 접속 URL (예: `redis://localhost:6379/0`)
- `INFLUX_URL`: InfluxDB 호스트명 (예: `http://localhost:8086`)
- `INFLUX_TOKEN`: InfluxDB 권한 토큰 (1차 틱 데이터 적재용)
- `INFLUX_ORG` / `INFLUX_BUCKET`: InfluxDB 대상 데이터베이스명
- `ACTIVE_EXCHANGES`: 구동할 거래소 모듈 스위치 (예: `upbit,coinbase`)
- `ENCRYPTION_MASTER_KEY`: **[주의]** API Secret Key를 DB에 저장할 때 사용하는 32바이트 AES-GCM 암호화 키입니다. 반드시 32자리를 설정해야 합니다. (예: `12345678901234567890123456789012`)

---

## 🧪 테스트 (Test)

### 1. 로컬 샌드박스 통합 테스트 (Local E2E Sandbox)

Magnolia의 핵심 모듈(AI Daemon, Feed Handler, Trading Node)이 프로그램 전체 흐름에서 어떻게 상호작용하는지 로컬 환경에서 안전하게 체험해 볼 수 있는 모의 테스트 환경입니다. **실제 프로덕션 배포 전, 가장 먼저 실행해보는 것을 강력히 권장합니다.**

### Step 1.1: 인프라 구동 (Docker Compose 활용)
시스템에 Docker가 설치되어 있다면, 프로젝트 최상단의 `docker-compose.yml` 리소스를 사용하여 인프라를 한 줄로 가동할 수 있습니다.
```bash
# 프로젝트 최상단 디렉토리에서 실행
docker compose up -d
```

### Step 1.2: 샌드박스 테스트 실행
Go 백엔드 명령어로 통합 테스트를 가동합니다.
```bash
cd backend
go run ./cmd/sandbox/main.go
```
명령어를 실행하면 12초간 시뮬레이션이 진행되며, `[Feed Handler]`, `[AI Daemon]`, `[Node Engine]`이 상호작용하여 데이터를 만들고 전략(`[SIGNAL]`)을 골라내는 과정을 라이브로 볼 수 있습니다.

### Step 1.3: 테스트 종료
확인이 끝났다면 리소스를 정리합니다.
```bash
# 컨테이너 종료 및 삭제
docker compose down
```

---

## 🚀 배포 (Deploy)

### 1. 베어메탈(자체 서버) 프로덕션 구동

단일 리눅스 또는 윈도우 서버(VM)에 직접 Magnolia 데몬을 구동하여 실제 트레이딩을 수행하는 배포 방식입니다.

#### Step 1.1: 서버용 인프라 구성
앞선 샌드박스의 `docker compose up -d` 환경을 프로덕션으로 그대로 유지하셔도 무방하며, 관리상의 이유로 개별 컨테이너로 각각 띄우길 원한다면 아래 명령어를 활용하세요.
```bash
# Redis 실행
docker run -d --name magnolia-redis -p 6379:6379 redis:alpine
# PostgreSQL 실행
docker run -d --name magnolia-postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=magnolia_test -p 5432:5432 postgres:15-alpine
# InfluxDB 실행
docker run -d --name magnolia-influx -p 8086:8086 influxdb:2.0
```

#### Step 1.2: 프로덕션 백엔드 빌드
```bash
cd backend
go mod tidy
go build -o bin/feed ./cmd/feed
go build -o bin/node ./cmd/node
```

#### Step 1.3: 운영 데몬 실행 (Feed Handler & Node)
데이터 수집 전용 서버 `Feed`와 분산 트레이딩 봇 `Node`를 백그라운드로 실행합니다 (추후 `systemd` 서비스 등록 권장).

터미널 A (Feed Handler 시작):
```bash
export REDIS_URL="redis://localhost:6379/0"
export INFLUX_URL="http://localhost:8086"
export INFLUX_TOKEN="my-token"
./bin/feed
```

터미널 B (Trading Node 시작):
```bash
export REDIS_URL="redis://localhost:6379/0"
export POSTGRES_DSN="host=localhost user=postgres password=postgres dbname=magnolia_test port=5432 sslmode=disable TimeZone=Asia/Seoul"
export ENCRYPTION_MASTER_KEY="12345678901234567890123456789012"
export STATIC_DIR="../frontend/dist"  # 빌드된 프론트엔드 경로 (선택)
./bin/node
```

> **💡 프론트엔드 서빙(Serving) 안내**
> - **[개발 환경]**: `frontend` 풀더에서 `npm run dev` 명령어를 사용하면 Vite 개발 서버가 HMR(Hot Module Replacement)을 지원하며 `:8080` 포트로 화면을 띄워줍니다.
> - **[운영 환경 (Production)]**: Nginx 없이 단독 배포할 수 있도록 백엔드(`cmd/node`)가 만들어져 있습니다. 운영 배포 시에는 프론트엔드를 `npm run build`로 빌드한 뒤, 생성된 `dist` 폴더를 `STATIC_DIR` 경로에 맞춰 연결하고 백엔드만 켜면 **백엔드(:8081 포트)** 에서 내장 웹 서버 기능으로 프론트엔드 화면까지 통합하여 한 번에 서비스됩니다.

---

### 2. 클라우드 (AWS / GCP) 배포 가이드

#### Step 2.1: 관리형 데이터베이스(Managed Database) 프로비저닝 가이드

분산 트레이딩 환경에서 데이터베이스는 글로벌 락(Lock)과 동기화를 담당하는 가장 중요한 심장부입니다. 데이터 유실 및 단일 장애점(SPOF)을 막기 위해 클라우드 벤더사에서 Multi-AZ 기능과 자동 자동 백업/복구를 100% 지원하는 **완전 관리형 서비스(Fully Managed Service)를 세팅하는 것을 강력히 권장**합니다.

**1) PostgreSQL 15+ (상태 저장 및 웹소켓 Pub/Sub 백본)**
- **역할**: 사용자 API 키 암호화 저장, 액티브 포지션 트래킹, 로드밸런싱된 프론트엔드 연동용 웹소켓 1:1 푸시 이벤트 처리(`pg_notify`).
- **AWS 환경**:
  - **Amazon RDS for PostgreSQL** 또는 **Amazon Aurora PostgreSQL**을 활용합니다.
  - **권장 세팅**: `Multi-AZ Deployment`를 필수로 활성화하여 마스터 노드 장애 시 스탠바이 노드로 자동 페일오버되게 구성합니다. 트레이딩 중 서버 크래시 복기(Crash Recovery)의 원천이 되므로 스토리지 암호화(KMS)와 자동 백업(최소 7일)도 켜두는 게 좋습니다.
- **GCP 환경**: 
  - **Cloud SQL for PostgreSQL**을 활용합니다.
  - **권장 세팅**: `High Availability (고가용성)` 옵션을 켜서 Zonal 장애에 대비합니다.

**2) Redis 7+ (초고속 마켓 데이터 브로커)**
- **역할**: 단일 Feed Handler(시세 수집단)가 거래소에서 받은 틱 데이터를 내부망에 있는 수십~수백 개의 봇(Node) 프로세스들에게 0ms 지연시간으로 브로드캐스팅(`Pub/Sub`).
- **AWS 환경**: 
  - **Amazon ElastiCache for Redis** 를 사용합니다.
  - **권장 세팅**: 속도가 가장 최우선이므로 Cluster Mode는 끄고(단일 파티션/Multi Replica 구조 사용), **반드시 Node 백엔드 봇 인스턴스들과 완전히 동일한 VPC 서브넷과 가용 영역(AZ)에 배치**하여 네트워크 지연을 극단적으로 줄이세요. 시세 데이터는 휘발성이므로 `Snapshot/Backup` 기능은 비용 절감을 위해 꺼두셔도 됩니다.
- **GCP 환경**: 
  - **Memorystore for Redis** 를 사용합니다. 동일 리전(Region) 구성 필수.

**3) InfluxDB v2 (딥러닝 자산 적재기)**
- **역할**: 밀리초 단위 단위의 Ticker(체결가)와 호가창(Orderbook)을 저장해, 추후 Python AI 모델(`PremiumForecaster`)이 읽어갈 초정밀 시계열 저장소입니다.
- **클라우드 구축 권장안**: 
  - 시계열 DB는 관리형 요금이 상당히 비싼 경우가 많습니다. 비용 대비 성능 최적화를 위해 **AWS EC2 (또는 GCP GCE) 가상머신을 단독으로 하나 프로비저닝** 한 뒤, **SSD 기반의 EBS (또는 Persistent Disk) 볼륨을 마운트** 시키고 그 위에 공식 Docker 이미지를 띄워 직접(Hybrid) 운영하는 방식을 실무적으로 가장 추천합니다.

#### Step 2.2: Docker Containerization
로컬에서 빌드된 바이너리를 클라우드로 배포할 수 있도록 Docker 이미지화합니다. 배포 파이프라인(Github Actions 등)에 아래 과정을 등록하세요.
```bash
# Node 이미지 빌드 및 ECR/GCR 푸시
docker build -t gcr.io/my-project/magnolia-node:latest -f deploy/Dockerfile.node .
docker push gcr.io/my-project/magnolia-node:latest

# Feed 이미지 빌드 및 푸시
docker build -t gcr.io/my-project/magnolia-feed:latest -f deploy/Dockerfile.feed .
docker push gcr.io/my-project/magnolia-feed:latest
```

#### Step 2.3: Kubernetes (EKS / GKE) 매니페스트 배포

Magnolia는 철저한 **Stateless (무상태)** 로 설계되었기에 몇 천 대의 Pod를 띄워도 에러 없이 스케일 아웃이 가능합니다.

- **Feed Handler (시세 봇)**: 거래소의 Rate Limit에 걸리지 않도록 항상 **Singleton (replicas: 1)** 구조로만 띄워야 합니다.
- **Nodes (트레이딩 봇)**: 트래픽에 맞춰 **N개 (replicas: 3+)** 로 유동적으로 띄웁니다.
- **무한 배수 (Infinite Draining) 방어**: 쿠버네티스 롤링 업데이트 시 기존에 매매중(Active) 이었던 봇이 칼같이 죽으면 자본 청산 리스크에 빠집니다. 매니페스트에 `terminationGracePeriodSeconds`를 매우 길게 잡아두어야 Magnolia의 `SIGTERM Draining` 후크가 완벽히 포지션을 마감하고 스스로 죽을 때까지 시스템이 안전하게 대기합니다.

```yaml
# deploy/node-deployment.yaml 예시
apiVersion: apps/v1
kind: Deployment
metadata:
  name: magnolia-node
spec:
  replicas: 3   # 언제든 무한대로 스케일 아웃 가능
  template:
    spec:
      terminationGracePeriodSeconds: 86400  # [중요] 봇이 물린 포지션을 탈출할때까지 최대 24시간 보장
      containers:
      - name: node
        image: gcr.io/my-project/magnolia-node:latest
        env:
        - name: POSTGRES_DSN
          valueFrom:
            secretKeyRef:
              name: db-secrets
              key: dsn
        # Redis 연결 세팅 등 후술
```

#### Step 2.4: Load Balancing 및 WebSocket 구성 연동
AWS ALB (`Application Load Balancer`) 또는 GCP `Global Cloud Load Balancer` 를 매니페스트 Ingress 로 부착합니다.
Magnolia 백엔드 프레임워크는 PostgreSQL `LISTEN / NOTIFY` 기반 1:1 채팅방 Pub/Sub 모델을 커스텀 구축해 놓았습니다. 수십 개의 `Node` 가 스위칭되어도 타겟팅 푸시가 완벽히 적용되므로 **Sticky Session (세션 고정) 기능이나 Redis WebSocket Adapter를 서드파티로 부착할 필요가 없습니다.** 단순히 트래픽만 라운드 로빈(Round Robin)으로 분산시키면 됩니다.
