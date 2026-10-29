/**
 * Phase 12 Full-Stack & Real Headless Chromium Browser E2E Test Suite
 *
 * Runs against the isolated `magnolia_test` Docker stack:
 * - Backend / Frontend Node Server: http://test-node:8080 (or E2E_BASE_URL)
 * - W3C WebDriver Headless Chromium: http://test-browser:4444/wd/hub (or E2E_WEBDRIVER_URL)
 */

const BASE_URL = process.env.E2E_BASE_URL || 'http://test-node:8081';
const WEBDRIVER_URL = process.env.E2E_WEBDRIVER_URL || 'http://test-browser:4444/wd/hub';

function assert(cond, msg) {
  if (!cond) {
    throw new Error(`❌ [E2E ASSERTION FAILED] ${msg}`);
  }
}

async function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

async function apiRequest(path, { method = 'GET', token = '', body = null } = {}) {
  const headers = { 'Content-Type': 'application/json' };
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(`${BASE_URL}${path}`, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined
  });
  const text = await res.text();
  let data = null;
  try {
    data = JSON.parse(text);
  } catch {
    data = text;
  }
  return { status: res.status, data };
}

/**
 * Reads an SSE endpoint until a target event type arrives or timeout expires.
 */
async function waitForSseEvent(path, targetEvent, timeoutMs = 8000) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const res = await fetch(`${BASE_URL}${path}`, {
      headers: { Accept: 'text/event-stream' },
      signal: controller.signal
    });
    assert(res.status === 200, `Expected SSE 200 on ${path}, got ${res.status}`);
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const blocks = buffer.split('\n\n');
      buffer = blocks.pop() || '';
      for (const block of blocks) {
        let evtName = 'message';
        let dataLines = [];
        for (const line of block.split('\n')) {
          if (line.startsWith('event:')) {
            evtName = line.slice(6).trim();
          } else if (line.startsWith('data:')) {
            dataLines.push(line.slice(5).trim());
          }
        }
        if (evtName === targetEvent && dataLines.length > 0) {
          return JSON.parse(dataLines.join('\n'));
        }
      }
    }
    throw new Error(`SSE stream closed before receiving event '${targetEvent}'`);
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}

/**
 * Minimal zero-dependency W3C WebDriver client for Headless Chromium (`selenium/standalone-chromium`).
 */
class ChromiumDriver {
  constructor(hubUrl) {
    this.hubUrl = hubUrl;
    this.sessionId = null;
  }

  async start() {
    const res = await fetch(`${this.hubUrl}/session`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        capabilities: {
          alwaysMatch: {
            browserName: 'chrome',
            'goog:chromeOptions': {
              args: ['--headless=new', '--no-sandbox', '--disable-dev-shm-usage', '--window-size=1440,900']
            }
          }
        }
      })
    });
    const json = await res.json();
    this.sessionId = json.value?.sessionId || json.sessionId;
    assert(this.sessionId, `Failed to start WebDriver Chromium session: ${JSON.stringify(json)}`);
  }

  async quit() {
    if (!this.sessionId) return;
    await fetch(`${this.hubUrl}/session/${this.sessionId}`, { method: 'DELETE' }).catch(() => {});
    this.sessionId = null;
  }

  async navigate(url) {
    await fetch(`${this.hubUrl}/session/${this.sessionId}/url`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url })
    });
  }

  async execute(script, args = []) {
    const res = await fetch(`${this.hubUrl}/session/${this.sessionId}/execute/sync`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ script, args })
    });
    const json = await res.json();
    return json.value;
  }

  async waitForText(expectedSubstring, timeoutMs = 8000) {
    const deadline = Date.now() + timeoutMs;
    let lastBody = '';
    while (Date.now() < deadline) {
      lastBody = (await this.execute('return document.body ? document.body.innerText : "";')) || '';
      if (lastBody.includes(expectedSubstring)) {
        return lastBody;
      }
      await sleep(250);
    }
    throw new Error(
      `Timed out waiting for text "${expectedSubstring}" in Chromium DOM.\nCurrent DOM text:\n${lastBody.slice(0, 800)}`
    );
  }
}

async function runFullLifecycleE2E() {
  console.log('================================================================');
  console.log('🚀 Starting Phase 12 Full-Stack & Headless Chromium E2E Suite...');
  console.log('================================================================');

  // 1. Register a fresh user on the isolated test stack
  const email = `e2e_${Date.now()}@magnolia.test`;
  const password = 'e2ePassword123!';
  console.log(`\n[Step 1] Registering isolated E2E user: ${email}`);
  const regRes = await apiRequest('/api/auth/register', {
    method: 'POST',
    body: { email, password, region: 'kr' }
  });
  assert(regRes.status === 201 && regRes.data?.token, `Register failed: ${JSON.stringify(regRes)}`);
  const token = regRes.data.token;
  const userId = regRes.data.user?.id;
  console.log(`  ✅ Registered user_id=${userId} with JWT token.`);

  // 2. Verify non-zero default thresholds are auto-seeded in Postgres
  console.log('\n[Step 2] Verifying non-zero default trading & risk parameters (/api/settings/parameters)');
  const paramRes = await apiRequest('/api/settings/parameters', { token });
  assert(paramRes.status === 200, `Expected 200 from /api/settings/parameters, got ${paramRes.status}`);
  assert(
    Number(paramRes.data.MinTargetProfit) === 1.5 &&
      Number(paramRes.data.MaxStopLoss) === -5 &&
      Number(paramRes.data.CapitalLimitUsd) === 50000 &&
      Number(paramRes.data.RebalanceTriggerSkew) === 0.2 &&
      Number(paramRes.data.RebalanceTargetSkew) === 0.5,
    `Expected non-zero seeded parameters (1.5 / -5 / 50000 / 0.2 / 0.5), got: ${JSON.stringify(paramRes.data)}`
  );
  console.log(
    `  ✅ Default parameters verified: min_profit=${paramRes.data.MinTargetProfit}%, stop_loss=${paramRes.data.MaxStopLoss}%, cap=$${paramRes.data.CapitalLimitUsd}, skew=${paramRes.data.RebalanceTriggerSkew}->${paramRes.data.RebalanceTargetSkew}`
  );

  // Wait briefly for test-engine to pop the new user from the dangling queue and start its goroutines
  console.log('\n[Step 3] Waiting for Engine goroutine pickup & verifying Dashboard SSE (/api/ws/dashboard)');
  await sleep(2000);
  const portfolioEvt = await waitForSseEvent(
    `/api/ws/dashboard?mode=PAPER&token=${encodeURIComponent(token)}`,
    'portfolio',
    8000
  );
  assert(
    Array.isArray(portfolioEvt.exchange_balances) && portfolioEvt.exchange_balances.length >= 2,
    `Expected >= 2 exchange_balances in portfolio SSE, got: ${JSON.stringify(portfolioEvt)}`
  );
  console.log(
    `  ✅ Dashboard SSE 'portfolio' event received: total_equity_usd=$${portfolioEvt.total_equity_usd}, exchanges=${portfolioEvt.exchange_balances.map((e) => `${e.name}(${e.quote_currency})`).join(', ')}`
  );

  // 4. Verify Opportunities SSE & Price Offset Simulator (+4.0% on UPBIT)
  console.log('\n[Step 4] Applying +4.0% Price Offset Simulator on UPBIT & verifying /api/ws/opportunities SSE');
  const simRes = await apiRequest('/api/settings/sim_offset', {
    method: 'POST',
    token,
    body: { exchange: 'UPBIT', target_offset_pct: 4.0, ramp_minutes: 0 }
  });
  assert(simRes.status === 200, `POST /api/settings/sim_offset failed: ${JSON.stringify(simRes)}`);

  const rawOppsEvt = await waitForSseEvent(
    `/api/ws/opportunities?mode=PAPER&token=${encodeURIComponent(token)}`,
    'raw_opportunities',
    10000
  );
  assert(
    Array.isArray(rawOppsEvt) && rawOppsEvt.length > 0,
    `Expected non-empty raw_opportunities array from SSE, got: ${JSON.stringify(rawOppsEvt)}`
  );
  assert(
    rawOppsEvt.some((o) => String(o.base_symbol).includes('BTC') && String(o.base_exchange).toUpperCase() === 'UPBIT'),
    `Expected BTC UPBIT->COINBASE pair in raw_opportunities, got: ${JSON.stringify(rawOppsEvt)}`
  );
  console.log(
    `  ✅ Opportunities SSE 'raw_opportunities' received (${rawOppsEvt.length} pairs). Top pair: ${rawOppsEvt[0].base_symbol} (${rawOppsEvt[0].base_exchange}->${rawOppsEvt[0].foreign_exchange}) spread=${Number(rawOppsEvt[0].premium_pct).toFixed(2)}%`
  );

  // 5. Execute Manual Position Entry (POST /api/positions/force_enter) & Verify Active Position
  // Keep +4.0% sim_offset active and match expected_roi to current spread (~0.039) so PositionMonitor holds it open
  const currentPremDecimal = Number(rawOppsEvt[0].premium_pct || 3.9) / 100.0;
  console.log('\n[Step 5] Executing manual position entry (POST /api/positions/force_enter) in PAPER mode');
  const enterRes = await apiRequest('/api/positions/force_enter', {
    method: 'POST',
    token,
    body: {
      mode: 'PAPER',
      base_exchange: 'UPBIT',
      foreign_exchange: 'COINBASE',
      base_symbol: 'BTC-KRW',
      foreign_symbol: 'BTC-USD',
      strategy: 'Margin-Spot (A)',
      expected_roi: currentPremDecimal,
      investment_usd: 1500
    }
  });
  assert(
    enterRes.status === 200 || enterRes.status === 201,
    `POST /api/positions/force_enter failed: ${JSON.stringify(enterRes)}`
  );

  let activePositions = [];
  for (let i = 0; i < 12; i++) {
    await sleep(500);
    const actRes = await apiRequest('/api/positions/active?mode=PAPER', { token });
    if (Array.isArray(actRes.data) && actRes.data.length > 0) {
      activePositions = actRes.data;
      break;
    }
  }
  assert(activePositions.length > 0, 'Expected active position in /api/positions/active?mode=PAPER after force_enter');
  const openedPos = activePositions[0];
  console.log(
    `  ✅ Active position created: ID=${openedPos.PositionID}, Pair=${openedPos.BaseSymbol}/${openedPos.ForeignSymbol}, Invested=$${openedPos.InvestmentUSD}`
  );

  // 6. Test Bot Status Transition to DRAIN (position remains open)
  console.log('\n[Step 6] Testing Bot Status transition to DRAIN while holding open position');
  const drainRes = await apiRequest('/api/settings/bot_status', {
    method: 'POST',
    token,
    body: { mode: 'PAPER', action: 'DRAIN' }
  });
  assert(
    drainRes.status === 200 && drainRes.data.paper_bot_status === 'DRAINING',
    `Expected DRAINING status, got: ${JSON.stringify(drainRes)}`
  );
  const actAfterDrain = await apiRequest('/api/positions/active?mode=PAPER', { token });
  assert(
    Array.isArray(actAfterDrain.data) && actAfterDrain.data.length === 1,
    'Expected position to remain open during DRAIN mode'
  );
  console.log('  ✅ Bot transitioned to DRAINING and open position was preserved.');

  // 7. Real Headless Chromium Browser DOM Verification (while position is OPEN, then after CLOSE)
  console.log('\n[Step 7] Launching Headless Chromium via W3C WebDriver for real browser DOM verification...');
  const browser = new ChromiumDriver(WEBDRIVER_URL);
  await browser.start();
  try {
    // 7a. Open /auth, inject auth token & language='ko' into localStorage, and navigate to /dashboard
    await browser.navigate(`${BASE_URL}/auth`);
    await browser.waitForText('Magnolia');
    await browser.execute(
      `localStorage.setItem('magnolia_token', arguments[0]);
       localStorage.setItem('magnolia_lang', 'ko');
       localStorage.setItem('magnolia_view_mode', 'PAPER');`,
      [token]
    );

    await browser.navigate(`${BASE_URL}/dashboard`);
    await browser.waitForText('UPBIT 가용 원화 (KRW)');
    await browser.waitForText('COINBASE 가용 달러 (USD)');
    await browser.waitForText('BTC');
    await browser.waitForText('UPBIT → COINBASE');
    console.log('  ✅ [Chromium DOM] /dashboard rendered dynamic exchange balances & active BTC position!');

    // 7b. Verify KO -> EN -> KO language switch in Chromium DOM
    await browser.execute(
      `const btns = Array.from(document.querySelectorAll('button'));
       const langBtn = btns.find(b => b.textContent.trim() === 'KO');
       if (langBtn) langBtn.click();`
    );
    await browser.waitForText('Market & Simulator');
    await browser.execute(
      `const btns = Array.from(document.querySelectorAll('button'));
       const langBtn = btns.find(b => b.textContent.trim() === 'EN');
       if (langBtn) langBtn.click();`
    );
    await browser.waitForText('마켓 & 시뮬레이터');
    console.log('  ✅ [Chromium DOM] Header KO <-> EN language toggle verified!');

    // 7c. Navigate to /opportunities and verify real-time opportunity rows are rendered in the DOM
    await browser.navigate(`${BASE_URL}/opportunities`);
    await browser.waitForText('거래소 간 실시간 프리미엄 목록');
    await browser.waitForText('BTC');
    await browser.waitForText('UPBIT (BTC-KRW) → COINBASE (BTC-USD)');
    console.log('  ✅ [Chromium DOM] /opportunities rendered live BTC cross-exchange premium row!');

    // 7d. Navigate to /settings?tab=params and verify non-zero risk parameter inputs in the DOM
    await browser.navigate(`${BASE_URL}/settings?tab=params`);
    await browser.waitForText('수익률 및 리스크 관리 기준');
    await sleep(600);
    const inputValues = await browser.execute(
      `return Array.from(document.querySelectorAll('input[type="number"]')).map(el => Number(el.value));`
    );
    assert(
      Array.isArray(inputValues) && inputValues.includes(1.5) && inputValues.includes(-5) && inputValues.includes(50000),
      `Expected non-zero input values [1.5, -5, 50000, ...] in /settings?tab=params DOM, got: ${JSON.stringify(inputValues)}`
    );
    console.log(`  ✅ [Chromium DOM] /settings?tab=params rendered non-zero inputs: ${JSON.stringify(inputValues)}`);

    // 8. Close the active position and verify it appears in /api/positions/history and /settings?tab=history DOM
    console.log('\n[Step 8] Closing active position (DELETE /api/positions/active/:id) & verifying Trade History');
    const closeRes = await apiRequest(`/api/positions/active/${openedPos.PositionID}?mode=PAPER`, {
      method: 'DELETE',
      token
    });
    assert(closeRes.status === 200, `DELETE /api/positions/active failed: ${JSON.stringify(closeRes)}`);

    let historyList = [];
    for (let i = 0; i < 12; i++) {
      await sleep(500);
      const histRes = await apiRequest('/api/positions/history?mode=PAPER', { token });
      if (Array.isArray(histRes.data) && histRes.data.length > 0) {
        historyList = histRes.data;
        break;
      }
    }
    assert(historyList.length > 0, 'Expected closed position in /api/positions/history?mode=PAPER');
    console.log(`  ✅ Closed position recorded in history: ID=${historyList[0].PositionID}, Status=${historyList[0].Status}`);

    await browser.navigate(`${BASE_URL}/settings?tab=history`);
    await browser.waitForText('완료된 아비트라지 거래 내역');
    await browser.waitForText('BTC');
    await browser.waitForText('UPBIT → COINBASE');
    console.log('  ✅ [Chromium DOM] /settings?tab=history rendered closed trade row!');
  } finally {
    await browser.quit();
  }

  console.log('\n================================================================');
  console.log('🎉 ALL FULL-STACK & HEADLESS CHROMIUM E2E TESTS PASSED (100%)!');
  console.log('================================================================\n');
}

runFullLifecycleE2E().catch((err) => {
  console.error(err);
  process.exit(1);
});
