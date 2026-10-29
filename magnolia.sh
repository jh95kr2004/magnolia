#!/usr/bin/env bash
# ==============================================================================
# 🌸 Magnolia Global Cross-Exchange Arbitrage Unified Control Script
# Usage:
#   ./magnolia.sh start [domain]    # Build & start all services + configure nginx
#   ./magnolia.sh stop              # Gracefully stop all services (preserves DB volumes)
#   ./magnolia.sh restart [domain]  # Restart all services
#   ./magnolia.sh status            # Show health of Docker containers, AI GPU Daemon & Nginx
#   ./magnolia.sh logs [service]    # Tail logs (e.g. './magnolia.sh logs', './magnolia.sh logs ai')
#   ./magnolia.sh ssl [domain]      # Issue/attach Let's Encrypt HTTPS cert via Certbot
#   ./magnolia.sh reset             # Stop & wipe all database volumes (clean factory reset)
#   ./magnolia.sh test [target]     # Run tests: all (default) | ai | backend | frontend | e2e
# ==============================================================================

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

AI_PID_FILE="$ROOT_DIR/.magnolia_ai.pid"
DOMAIN_FILE="$ROOT_DIR/.magnolia_domain"
AI_LOG_DIR="$ROOT_DIR/ai_service/logs"
AI_LOG_FILE="$AI_LOG_DIR/ai_daemon.log"
NGINX_CONF_FILE="$ROOT_DIR/nginx.conf"
NGINX_SITE_AVAILABLE="/etc/nginx/sites-available/magnolia"
NGINX_SITE_ENABLED="/etc/nginx/sites-enabled/magnolia"

# Detect Python interpreter (prefer RTX 3090 torch_env if available)
if [ -x "/home/jh95kr2004/miniconda3/envs/torch_env/bin/python" ]; then
    PYTHON_BIN="/home/jh95kr2004/miniconda3/envs/torch_env/bin/python"
    PIP_BIN="/home/jh95kr2004/miniconda3/envs/torch_env/bin/pip"
elif command -v python3 >/dev/null 2>&1; then
    PYTHON_BIN="$(command -v python3)"
    PIP_BIN="$(command -v pip3 || echo "$PYTHON_BIN -m pip")"
else
    PYTHON_BIN="python"
    PIP_BIN="pip"
fi

# Colors
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_info()  { echo -e "${CYAN}[Magnolia]${NC} $*"; }
log_ok()    { echo -e "${GREEN}[  OK  ]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[ WARN ]${NC} $*"; }
log_err()   { echo -e "${RED}[ERROR ]${NC} $*"; }

resolve_domain() {
    local input_domain="${1:-}"
    if [ -n "$input_domain" ]; then
        echo "$input_domain" > "$DOMAIN_FILE"
        echo "$input_domain"
    elif [ -f "$DOMAIN_FILE" ]; then
        cat "$DOMAIN_FILE"
    else
        echo "_"
    fi
}

generate_nginx_conf() {
    local domain="$1"
    local ssl_cert="/etc/letsencrypt/live/${domain}/fullchain.pem"
    local ssl_key="/etc/letsencrypt/live/${domain}/privkey.pem"

    if [ "$domain" != "_" ] && [ -f "$ssl_cert" ] && [ -f "$ssl_key" ]; then
        log_info "Detected Let's Encrypt SSL certificate for ${domain}. Generating HTTPS + HTTP->HTTPS nginx.conf..."
        cat > "$NGINX_CONF_FILE" <<EOF
server {
    server_name ${domain};
    client_max_body_size 50M;

    # 1. Magnolia Unified Trading Node (React SPA Assets + REST/SSE API)
    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 86400;
        proxy_send_timeout 86400;
        chunked_transfer_encoding on;
    }

    # 2. Go Backend REST API & Server-Sent Events (SSE)
    location /api/ {
        proxy_pass http://127.0.0.1:8081/api/;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 86400;
        proxy_send_timeout 86400;
        chunked_transfer_encoding on;
    }

    # 3. Interactive Exchange Web Lending Browser (noVNC + WebSocket proxy)
    location /novnc/ {
        proxy_pass http://127.0.0.1:6080/;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_read_timeout 86400;
        proxy_send_timeout 86400;
    }

    listen 443 ssl;
    ssl_certificate ${ssl_cert};
    ssl_certificate_key ${ssl_key};
    include /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem;
}

server {
    if (\$host = ${domain}) {
        return 301 https://\$host\$request_uri;
    }
    listen 80;
    server_name ${domain};
    return 404;
}
EOF
    else
        log_info "Generating nginx.conf with server_name '${domain}'..."
        cat > "$NGINX_CONF_FILE" <<EOF
server {
    listen 80;
    server_name ${domain};
    client_max_body_size 50M;

    # 1. Magnolia Unified Trading Node (React SPA Assets + REST/SSE API)
    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 86400;
        proxy_send_timeout 86400;
        chunked_transfer_encoding on;
    }

    # 2. Go Backend REST API & Server-Sent Events (SSE)
    location /api/ {
        proxy_pass http://127.0.0.1:8081/api/;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 86400;
        proxy_send_timeout 86400;
        chunked_transfer_encoding on;
    }

    # 3. Interactive Exchange Web Lending Browser (noVNC + WebSocket proxy)
    location /novnc/ {
        proxy_pass http://127.0.0.1:6080/;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_read_timeout 86400;
        proxy_send_timeout 86400;
    }
}
EOF
    fi
}

apply_nginx_conf() {
    local domain="$1"
    generate_nginx_conf "$domain"

    if ! command -v nginx >/dev/null 2>&1; then
        log_warn "Nginx is not installed on this host. Skipping /etc/nginx registration."
        return 0
    fi

    # Skip sudo if /etc/nginx/sites-available/magnolia is already identical and linked
    if [ -f "$NGINX_SITE_AVAILABLE" ] && [ -L "$NGINX_SITE_ENABLED" ] && cmp -s "$NGINX_CONF_FILE" "$NGINX_SITE_AVAILABLE"; then
        log_ok "Nginx config for '${domain}' is already up to date at ${NGINX_SITE_ENABLED}."
        return 0
    fi

    local ssl_cert="/etc/letsencrypt/live/${domain}/fullchain.pem"
    local run_sudo=""
    if [ "$(id -u)" -ne 0 ]; then
        run_sudo="sudo"
    fi

    if [ "$(id -u)" -eq 0 ] || sudo -n true 2>/dev/null || [ -t 0 ]; then
        log_info "Applying Nginx config to ${NGINX_SITE_ENABLED} (enter sudo password if prompted)..."
        if $run_sudo cp "$NGINX_CONF_FILE" "$NGINX_SITE_AVAILABLE" && \
           $run_sudo ln -sf "$NGINX_SITE_AVAILABLE" "$NGINX_SITE_ENABLED" && \
           $run_sudo nginx -t && $run_sudo systemctl reload nginx; then
            log_ok "Nginx HTTP config reloaded for domain: ${domain}"

            # Automatically issue & attach Let's Encrypt HTTPS certificate if certbot is installed and cert doesn't exist yet
            if [ "$domain" != "_" ] && [ "$domain" != "localhost" ] && ! $run_sudo test -f "$ssl_cert" && command -v certbot >/dev/null 2>&1; then
                log_info "Issuing Let's Encrypt HTTPS certificate for ${domain} via Certbot..."
                if $run_sudo certbot --nginx -d "$domain" --non-interactive --agree-tos --register-unsafely-without-email --redirect; then
                    log_ok "HTTPS (SSL 443) certificate automatically issued and enabled for https://${domain}!"
                else
                    log_warn "Certbot HTTPS issuance failed or skipped. HTTP (http://${domain}) remains active."
                fi
            fi
        else
            log_warn "Skipped automatic Nginx reload (run './magnolia.sh start ${domain}' in your terminal to enter sudo password)."
        fi
    else
        log_warn "Updated ${NGINX_CONF_FILE} with server_name '${domain}'."
    fi
}

ensure_ai_deps() {
    log_info "Verifying Python dependencies in ${PYTHON_BIN}..."
    if ! "$PYTHON_BIN" -c "import torch, redis, influxdb_client, dotenv, joblib, sklearn, stable_baselines3, gymnasium" >/dev/null 2>&1; then
        log_info "Installing missing AI Service Python libraries..."
        $PIP_BIN install -q redis influxdb-client python-dotenv joblib scikit-learn stable-baselines3 gymnasium tqdm
    fi
    log_ok "AI Service Python dependencies verified."
}

start_ai_daemon() {
    mkdir -p "$AI_LOG_DIR"
    if [ -f "$AI_PID_FILE" ]; then
        local existing_pid
        existing_pid="$(cat "$AI_PID_FILE" 2>/dev/null || true)"
        if [ -n "$existing_pid" ] && kill -0 "$existing_pid" 2>/dev/null; then
            log_ok "AI Daemon is already running (PID: ${existing_pid})."
            return 0
        fi
        rm -f "$AI_PID_FILE"
    fi

    log_info "Starting RTX 3090 AI Prediction & RL Daemon (ai_service/app.py)..."
    (
        cd "$ROOT_DIR/ai_service"
        REDIS_URL="redis://localhost:6379/0" \
        INFLUX_URL="http://localhost:8086" \
        INFLUX_TOKEN="magnolia_admin_secret_token_123" \
        INFLUX_ORG="magnolia" \
        INFLUX_BUCKET="market_data" \
        nohup "$PYTHON_BIN" -u app.py >> "$AI_LOG_FILE" 2>&1 &
        echo $! > "$AI_PID_FILE"
    )
    sleep 1
    local new_pid
    new_pid="$(cat "$AI_PID_FILE" 2>/dev/null || true)"
    if [ -n "$new_pid" ] && kill -0 "$new_pid" 2>/dev/null; then
        log_ok "AI Daemon started in background (PID: ${new_pid}, Log: ai_service/logs/ai_daemon.log)."
    else
        log_err "AI Daemon failed to start. Check $AI_LOG_FILE"
        return 1
    fi
}

stop_ai_daemon() {
    if [ -f "$AI_PID_FILE" ]; then
        local pid
        pid="$(cat "$AI_PID_FILE" 2>/dev/null || true)"
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            log_info "Stopping AI Daemon (PID: ${pid})..."
            kill "$pid" 2>/dev/null || true
            sleep 1
            kill -9 "$pid" 2>/dev/null || true
            log_ok "AI Daemon stopped."
        fi
        rm -f "$AI_PID_FILE"
    fi
    # Clean up any stray ai_service/app.py process started by this user
    pkill -f "python.*app.py" 2>/dev/null || true
}

cmd_start() {
    local domain
    domain="$(resolve_domain "${1:-}")"
    echo "=================================================================="
    echo " 🌸 Starting Magnolia Global Arbitrage Stack (Domain: ${domain})"
    echo "=================================================================="

    ensure_ai_deps
    stop_ai_daemon

    log_info "Building & starting all 8 Docker Compose containers (Postgres, Redis, InfluxDB, noVNC, AI, Feed, Engine, Node)..."
    docker compose up -d --build
    log_ok "All 8 Docker containers (including AI Service) are up and managed by Docker."

    apply_nginx_conf "$domain"

    echo ""
    cmd_status
}

cmd_stop() {
    echo "=================================================================="
    echo " 🛑 Stopping Magnolia Global Arbitrage Stack (Preserving DB Data)"
    echo "=================================================================="
    stop_ai_daemon
    log_info "Stopping all Docker Compose containers..."
    docker compose down
    log_ok "All services stopped cleanly. Database volumes are preserved."
}

cmd_status() {
    local domain
    domain="$(resolve_domain "")"
    echo "=================================================================="
    echo " 📊 Magnolia System Health Status"
    echo "=================================================================="
    echo -e " • Configured Domain : ${GREEN}${domain}${NC}"

    if docker ps --format '{{.Names}}' | grep -q '^magnolia_ai$'; then
        echo -e " • AI Service (Docker): ${GREEN}RUNNING${NC} (container: magnolia_ai)"
    else
        echo -e " • AI Service (Docker): ${RED}STOPPED${NC}"
    fi

    if curl -fsS "http://127.0.0.1:8081/" >/dev/null 2>&1; then
        echo -e " • Web/API Node(8081): ${GREEN}HEALTHY${NC} (http://127.0.0.1:8081)"
    else
        echo -e " • Web/API Node(8081): ${YELLOW}STARTING / UNREACHABLE${NC}"
    fi

    echo "------------------------------------------------------------------"
    docker compose ps
    echo "=================================================================="
    if [ "$domain" != "_" ]; then
        echo -e " 🌐 Web Dashboard URL : ${GREEN}http://${domain}${NC} (or https://${domain} if SSL is enabled)"
        echo -e " 🖥️  noVNC Lending URL : ${GREEN}http://${domain}/novnc/${NC}"
    else
        echo -e " 🌐 Local Web URL     : ${GREEN}http://localhost:8081${NC}"
    fi
}

cmd_logs() {
    local target="${1:-}"
    if [ -n "$target" ]; then
        docker compose logs -f --tail=100 "$target"
    else
        docker compose logs -f --tail=50
    fi
}

cmd_ssl() {
    local domain
    domain="$(resolve_domain "${1:-}")"
    if [ "$domain" = "_" ]; then
        log_err "Please provide a domain name: ./magnolia.sh ssl magnolia.hooneken.com"
        exit 1
    fi
    apply_nginx_conf "$domain"
    log_info "Requesting/configuring Let's Encrypt SSL certificate for ${domain} via Certbot..."
    sudo certbot --nginx -d "$domain"
    log_ok "SSL certificate configured for https://${domain}"
}

cmd_reset() {
    echo -e "${RED}WARNING: This will stop all services and PERMANENTLY DELETE all database volumes (users, positions, paper balances, noVNC profile).${NC}"
    read -r -p "Are you sure you want to proceed? [y/N]: " ans
    if [[ "$ans" =~ ^[Yy]$ ]]; then
        stop_ai_daemon
        docker compose down -v
        log_ok "All containers and volumes have been wiped."
    else
        log_info "Reset cancelled."
    fi
}

cleanup_test_stack() {
    log_info "Tearing down isolated test stack (magnolia_test)..."
    docker compose -p magnolia_test -f "$ROOT_DIR/docker-compose.test.yml" down -v --remove-orphans >/dev/null 2>&1 || true
}

run_test_ai() {
    echo "------------------------------------------------------------------"
    log_info "🧠 [1/4] Running AI Service Unit Tests (pytest)..."
    echo "------------------------------------------------------------------"
    (
        cd "$ROOT_DIR/ai_service"
        "$PYTHON_BIN" -m pytest -v tests/
    )
    log_ok "AI Service Unit Tests PASSED!"
}

run_test_backend() {
    echo "------------------------------------------------------------------"
    log_info "⚙️  [2/4] Running Go Backend Unit & DB/Redis Integration Tests..."
    echo "------------------------------------------------------------------"
    docker compose -p magnolia_test -f "$ROOT_DIR/docker-compose.test.yml" up -d --wait test-postgres test-redis
    docker run --rm \
        --network magnolia_test_default \
        -v "$ROOT_DIR/backend:/app" \
        -v magnolia_gocache:/go/pkg/mod \
        -v magnolia_gobuild:/root/.cache/go-build \
        -w /app \
        -e TEST_POSTGRES_DSN="host=test-postgres user=magnolia_test password=magnolia_test dbname=magnolia_test port=5432 sslmode=disable" \
        -e TEST_REDIS_URL="redis://test-redis:6379/0" \
        golang:1.25-alpine \
        go test -p 1 -v ./internal/exchange ./internal/strategy ./internal/db/postgres ./internal/api
    log_ok "Go Backend Unit & Integration Tests PASSED!"
}

run_test_frontend() {
    echo "------------------------------------------------------------------"
    log_info "🎨 [3/4] Running Frontend Component & SSE Unit Tests (Vitest)..."
    echo "------------------------------------------------------------------"
    docker run --rm \
        -v "$ROOT_DIR/frontend:/app" \
        -w /app \
        node:20-alpine \
        npm test
    log_ok "Frontend Component & SSE Unit Tests PASSED!"
}

run_test_e2e() {
    echo "------------------------------------------------------------------"
    log_info "🌐 [4/4] Running Full-Stack & Headless Chromium Browser E2E Suite..."
    echo "------------------------------------------------------------------"
    log_info "Starting isolated magnolia_test stack (Postgres, Redis, Chromium, AI, Feed, Engine, Node)..."
    docker compose -p magnolia_test -f "$ROOT_DIR/docker-compose.test.yml" up -d
    log_info "Waiting for test-node (http://127.0.0.1:18081) and test-browser WebDriver to become ready..."
    for _ in $(seq 1 30); do
        if curl -fsS "http://127.0.0.1:18081/" >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
    sleep 3
    if ! docker run --rm \
        --network magnolia_test_default \
        -v "$ROOT_DIR/frontend:/app" \
        -w /app \
        -e E2E_BASE_URL="http://test-node:8081" \
        -e E2E_WEBDRIVER_URL="http://test-browser:4444/wd/hub" \
        node:20-alpine \
        node e2e/full_lifecycle.e2e.mjs; then
        log_err "E2E failed! Dumping test-engine and test-node logs:"
        docker compose -p magnolia_test -f "$ROOT_DIR/docker-compose.test.yml" logs --tail=60 test-engine test-node || true
        return 1
    fi
    log_ok "Full-Stack & Headless Chromium Browser E2E Tests PASSED!"
}

cmd_test() {
    local target="${1:-all}"
    trap cleanup_test_stack EXIT

    case "$target" in
        ai)
            run_test_ai
            ;;
        backend|go)
            run_test_backend
            ;;
        frontend|ui)
            run_test_frontend
            ;;
        e2e)
            run_test_e2e
            ;;
        all|"")
            run_test_ai
            run_test_backend
            run_test_frontend
            run_test_e2e
            echo "=================================================================="
            log_ok "🎉 ALL PHASE 12 TEST SUITES (AI + BACKEND + FRONTEND + E2E) PASSED!"
            echo "=================================================================="
            ;;
        *)
            log_err "Unknown test target '${target}'. Choose from: all | ai | backend | frontend | e2e"
            exit 1
            ;;
    esac
}

ACTION="${1:-help}"
shift || true

case "$ACTION" in
    start|up)
        cmd_start "${1:-}"
        ;;
    stop|down)
        cmd_stop
        ;;
    restart)
        cmd_stop
        cmd_start "${1:-}"
        ;;
    status|ps)
        cmd_status
        ;;
    logs|log)
        cmd_logs "${1:-}"
        ;;
    ssl|certbot)
        cmd_ssl "${1:-}"
        ;;
    reset|clean)
        cmd_reset
        ;;
    test|tests)
        cmd_test "${1:-all}"
        ;;
    *)
        echo "Usage: ./magnolia.sh {start [domain]|stop|restart [domain]|status|logs [service|ai]|ssl [domain]|reset|test [all|ai|backend|frontend|e2e]}"
        exit 1
        ;;
esac
