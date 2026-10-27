import React, { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Activity,
  PieChart,
  TrendingUp,
  AlertOctagon,
  Zap,
  ShieldAlert,
  CheckCircle2,
  ArrowRight,
  Radio,
  Scale,
  Terminal,
  ExternalLink,
  FlaskConical,
  Play,
  Pause,
  RotateCcw,
  Sliders
} from 'lucide-react';
import { apiClient } from '../../services/api';
import { buildSseUrl, useAuth } from '../../contexts/AuthContext';
import { useToast } from '../../contexts/ToastContext';
import './Dashboard.css';

export const Dashboard = () => {
  const navigate = useNavigate();
  const { showToast, requestConfirm } = useToast();
  const { viewMode, switchViewMode, botStatuses, setBotStatuses } = useAuth();

  const [sseConnected, setSseConnected] = useState(false);
  const [lastSync, setLastSync] = useState(null);

  const [portfolio, setPortfolio] = useState({
    mode: viewMode,
    botStatus: 'RUNNING',
    paperBotStatus: 'RUNNING',
    liveBotStatus: 'STOPPED',
    paperUpbitKrw: 50000000,
    paperCoinbaseUsd: 35000,
    totalUsd: 0,
    availableUsd: 0,
    foreignShare: 0.5,
    rebalanceTriggerSkew: 0.2,
    rebalanceTargetSkew: 0.5,
    webLendingSessions: {},
    exchanges: []
  });

  const [simStatus, setSimStatus] = useState({
    exchange: 'UPBIT',
    start_offset_pct: 0,
    target_offset_pct: 0,
    current_offset_pct: 0,
    ramp_minutes: 0,
    progress_pct: 100,
    remaining_seconds: 0,
    is_ramping: false
  });

  const [simForm, setSimForm] = useState({
    exchange: 'UPBIT',
    targetOffsetPct: 3.5,
    rampMinutes: 3
  });
  const [simSubmitting, setSimSubmitting] = useState(false);
  const [showStopModal, setShowStopModal] = useState(false);
  const [statusUpdating, setStatusUpdating] = useState(false);

  const [activePositions, setActivePositions] = useState([]);
  const [topOpportunities, setTopOpportunities] = useState([]);
  const [eventLogs, setEventLogs] = useState([]);

  const appendEventLog = (type, message, level = 'info') => {
    setEventLogs(prev => [
      {
        id: Date.now() + Math.random(),
        time: new Date().toLocaleTimeString(),
        type,
        message,
        level
      },
      ...prev.slice(0, 29)
    ]);
  };

  useEffect(() => {
    setSseConnected(false);
    const dashboardSrc = new EventSource(buildSseUrl('/api/ws/dashboard', viewMode), { withCredentials: true });
    const oppsSrc = new EventSource(buildSseUrl('/api/ws/opportunities', viewMode), { withCredentials: true });

    dashboardSrc.onopen = () => setSseConnected(true);
    dashboardSrc.onerror = () => setSseConnected(false);

    dashboardSrc.addEventListener('portfolio', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data) {
          const rawExchanges = Array.isArray(data.exchange_balances)
            ? data.exchange_balances
            : Array.isArray(data.portfolio)
              ? data.portfolio
              : [];

          const mappedExchanges = rawExchanges.map(ex => ({
            name: ex.name,
            asset: ex.quote_currency || ex.asset || (ex.name === 'UPBIT' ? 'KRW' : 'USD'),
            nativeBalance: typeof ex.available_native === 'number' ? ex.available_native : 0,
            usdValue: typeof ex.available_usd === 'number' ? ex.available_usd : (ex.usdValue || 0),
            percent: typeof ex.percent === 'number' ? ex.percent : 0,
            webLendingOk: ex.web_lending_ok
          }));

          const pStatus = data.paper_bot_status || 'RUNNING';
          const lStatus = data.live_bot_status || 'STOPPED';

          setPortfolio({
            mode: data.mode || viewMode,
            botStatus: data.bot_status || (viewMode === 'PAPER' ? pStatus : lStatus),
            paperBotStatus: pStatus,
            liveBotStatus: lStatus,
            paperUpbitKrw: data.paper_upbit_krw || 50000000,
            paperCoinbaseUsd: data.paper_coinbase_usd || 35000,
            totalUsd: typeof data.total_equity_usd === 'number' ? data.total_equity_usd : 0,
            availableUsd: typeof data.available_balance === 'number'
              ? data.available_balance
              : (typeof data.available_usd === 'number' ? data.available_usd : 0),
            foreignShare: typeof data.foreign_share === 'number' ? data.foreign_share : 0.5,
            rebalanceTriggerSkew: data.rebalance_trigger_skew || 0.2,
            rebalanceTargetSkew: data.rebalance_target_skew || 0.5,
            webLendingSessions: data.web_lending_sessions || {},
            exchanges: mappedExchanges
          });

          setBotStatuses({
            PAPER: pStatus,
            LIVE: lStatus
          });

          if (data.sim_status) {
            setSimStatus(data.sim_status);
          }

          setLastSync(new Date());
          setSseConnected(true);
        }
      } catch (err) {}
    });

    dashboardSrc.addEventListener('positions', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data && Array.isArray(data.positions)) {
          setActivePositions(
            data.positions.map(p => ({
              id: p.PositionID || p.ID,
              isPaper: !!p.IsPaper,
              strategy: p.Type,
              baseExchange: p.BaseExchange,
              foreignExchange: p.ForeignExchange,
              pair: `${p.BaseSymbol} → ${p.ForeignSymbol}`,
              investedUsd: p.InvestmentUSD || 0,
              entrySpreadPct: ((p.EntrySpread ?? p.EntryPremium) || 0) * 100,
              expectedRoiPct: ((p.ExpectedROI ?? p.ExpectedFuturePrem) || 0) * 100,
              currentPnlUsd: typeof p.CurrentPnLUSD === 'number' ? p.CurrentPnLUSD : (p.RealizedProfitUSD || 0),
              currentPnlPct: typeof p.CurrentPnLPct === 'number' ? p.CurrentPnLPct : 0,
              status: p.Status || 'OPEN',
              openedAt: p.OpenedAt
            }))
          );
        }
      } catch (err) {}
    });

    dashboardSrc.addEventListener('notification', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data && data.message) {
          const t = data.type || 'ENGINE_EVENT';
          let toastType = 'info';
          if (t.includes('LOST') || t.includes('FAILED') || t.includes('EMERGENCY')) {
            toastType = 'critical';
          } else if (t.includes('OPENED') || t.includes('CLOSED') || t.includes('COMPLETED') || t.includes('SIM_')) {
            toastType = 'success';
          } else if (t.includes('QUEUED') || t.includes('REBALANCE') || t.includes('STATUS')) {
            toastType = 'warning';
          }
          showToast(`[${t}] ${data.message}`, toastType);
          appendEventLog(t, data.message, toastType);
        }
      } catch (err) {}
    });

    oppsSrc.addEventListener('filtered_opportunities', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (Array.isArray(data)) {
          const sorted = [...data]
            .filter(o => (o.expected_roi || 0) > 0)
            .sort((a, b) => (b.expected_roi || 0) - (a.expected_roi || 0))
            .slice(0, 4);
          setTopOpportunities(sorted);
        }
      } catch (err) {}
    });

    return () => {
      dashboardSrc.close();
      oppsSrc.close();
    };
  }, [viewMode, showToast, setBotStatuses]);

  const executeBotStatusChange = async (subAction) => {
    setStatusUpdating(true);
    try {
      const res = await apiClient.post('/settings/bot_status', {
        mode: viewMode,
        sub_action: subAction
      });
      if (res.data) {
        setBotStatuses({
          PAPER: res.data.paper_bot_status || portfolio.paperBotStatus,
          LIVE: res.data.live_bot_status || portfolio.liveBotStatus
        });
        setPortfolio(prev => ({
          ...prev,
          botStatus: res.data.status || prev.botStatus,
          paperBotStatus: res.data.paper_bot_status || prev.paperBotStatus,
          liveBotStatus: res.data.live_bot_status || prev.liveBotStatus
        }));
      }
      setShowStopModal(false);
      showToast(
        `${viewMode === 'PAPER' ? '모의매매(Paper)' : '실매매(Live)'} 엔진 상태가 [${subAction}] 모드로 변경되었습니다.`,
        'success'
      );
    } catch (err) {
      showToast(err.response?.data?.error || '엔진 상태 변경에 실패했습니다.', 'error');
    } finally {
      setStatusUpdating(false);
    }
  };

  const handleStopButtonClick = () => {
    if (activePositions.length > 0) {
      setShowStopModal(true);
    } else {
      executeBotStatusChange('STOP_AND_CLOSE');
    }
  };

  const applySimOffset = async (exchange, targetOffsetPct, rampMinutes) => {
    setSimSubmitting(true);
    try {
      const res = await apiClient.post('/settings/sim_offset', {
        exchange,
        target_offset_pct: parseFloat(targetOffsetPct),
        ramp_minutes: parseFloat(rampMinutes)
      });
      if (res.data?.sim_status) {
        setSimStatus(res.data.sim_status);
      }
      setSimForm({
        exchange,
        targetOffsetPct: parseFloat(targetOffsetPct),
        rampMinutes: parseFloat(rampMinutes)
      });
      showToast(
        `[가격 시뮬레이터] ${exchange} ${targetOffsetPct >= 0 ? '+' : ''}${targetOffsetPct}% (${rampMinutes}분 점진 변동) 적용 완료`,
        'success'
      );
    } catch (err) {
      showToast('시뮬레이터 설정 적용 실패: ' + (err.response?.data?.error || err.message), 'error');
    } finally {
      setSimSubmitting(false);
    }
  };

  const handleResetPaperSeed = () => {
    requestConfirm({
      title: '모의매매 가상 계좌 초기화 (Reset Paper Account)',
      message: `현재 열린 모의매매 포지션을 모두 정리하고 가상 시드머니를 초기 설정값(UPBIT ₩${portfolio.paperUpbitKrw.toLocaleString()} / COINBASE $${portfolio.paperCoinbaseUsd.toLocaleString()})으로 복원하시겠습니까?\n(실매매 포지션에는 아무런 영향을 주지 않습니다.)`,
      confirmText: '가상 시드 초기화 실행',
      danger: true,
      onConfirm: async () => {
        try {
          await apiClient.post('/settings/paper_reset', {
            upbit_krw: portfolio.paperUpbitKrw,
            coinbase_usd: portfolio.paperCoinbaseUsd
          });
          showToast('모의매매 가상 계좌 및 포지션이 초기화되었습니다.', 'success');
        } catch (err) {
          showToast('초기화 실패: ' + (err.response?.data?.error || err.message), 'error');
        }
      }
    });
  };

  const handleForceClose = (pos) => {
    requestConfirm({
      title: `Force Close Position (${pos.id})`,
      message: `Are you sure you want to immediately unwind ${pos.pair} (${pos.strategy}) in ${viewMode} mode?\n\nThe Go Engine will execute delta-neutral market orders on ${pos.baseExchange} & ${pos.foreignExchange} and repay any active coin loans.`,
      confirmText: 'Execute Market Unwind',
      danger: true,
      onConfirm: async () => {
        try {
          await apiClient.delete(`/positions/active/${pos.id}?mode=${viewMode}`);
          showToast(`Force close command dispatched to ${viewMode} Engine for ${pos.id}.`, 'warning');
          appendEventLog('FORCE_CLOSE_SENT', `Requested immediate unwind for ${pos.id} (${viewMode})`, 'warning');
        } catch (err) {
          showToast('Failed to dispatch close command: ' + (err.response?.data?.error || err.message), 'error');
        }
      }
    });
  };

  const handleForceEnter = (opp) => {
    const asset = opp.base_symbol?.split('-')[0] || opp.base_symbol;
    const route = `${opp.base_exchange} (${asset}) → ${opp.foreign_exchange}`;
    const roiPct = ((opp.expected_roi || 0) * 100).toFixed(2);
    const weightPct = ((opp.target_weight || 0.2) * 100).toFixed(1);

    requestConfirm({
      title: `Force Enter Arbitrage (${asset} — ${viewMode} Mode)`,
      message: `Mode: ${viewMode === 'PAPER' ? '🧪 모의매매 (Paper Engine)' : '⚡ 실매매 (Live Engine)'}\nRoute: ${route}\nStrategy: ${opp.strategy}\nExpected Net ROI: +${roiPct}%\nSuggested Allocation Weight: ${weightPct}%\n\nThis sends an immediate execution command to your ${viewMode} Engine goroutine.`,
      confirmText: `Execute ${viewMode} Hedge Entry`,
      danger: viewMode === 'LIVE',
      onConfirm: async () => {
        try {
          await apiClient.post('/positions/force_enter', { ...opp, mode: viewMode });
          showToast(`Force entry command sent to ${viewMode} Engine for ${route}.`, 'success');
          appendEventLog('FORCE_ENTER_SENT', `Dispatched ${opp.strategy} entry on ${route} (${viewMode})`, 'success');
        } catch (err) {
          showToast('Failed to dispatch entry: ' + (err.response?.data?.error || err.message), 'error');
        }
      }
    });
  };

  const disconnectedSessions = Object.entries(portfolio.webLendingSessions || {})
    .filter(([, valid]) => valid === false)
    .map(([ex]) => ex);

  const foreignSharePct = Math.max(0, Math.min(100, (portfolio.foreignShare || 0) * 100));
  const triggerPct = (portfolio.rebalanceTriggerSkew || 0.2) * 100;
  const targetPct = (portfolio.rebalanceTargetSkew || 0.5) * 100;

  const isSkewCritical = foreignSharePct <= triggerPct;
  const isSkewWarning = !isSkewCritical && foreignSharePct <= triggerPct + 8;

  const currentBotStatus = (portfolio.botStatus || (viewMode === 'PAPER' ? botStatuses?.PAPER : botStatuses?.LIVE) || 'RUNNING').toUpperCase();

  const getLifecycleStage = (status) => {
    const s = (status || 'OPEN').toUpperCase();
    if (s.includes('REPAY')) return 3;
    if (s.includes('TRANSFER')) return 2;
    return 1;
  };

  const formatRemainingTime = (sec) => {
    const s = Math.max(0, Math.round(sec || 0));
    const m = Math.floor(s / 60);
    const rem = s % 60;
    return m > 0 ? `${m}분 ${rem}초` : `${rem}초`;
  };

  return (
    <div className="dashboard-page animate-fade-in delay-100">
      {/* Header */}
      <header className="page-header mb-md flex-between">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-sm">
            <Activity className="text-accent-primary" /> Arbitrage Control Center
          </h1>
          <p className="text-secondary mt-xs">
            Independent Dual-Engine (Paper + Live) execution, 100% real exchange feeds, and delta-neutral skew telemetry.
          </p>
        </div>
        <div className="flex items-center gap-md">
          <div className={`sse-status-pill ${sseConnected ? 'online' : 'offline'}`}>
            <Radio size={13} className={sseConnected ? 'animate-pulse' : ''} />
            <span>{sseConnected ? `${viewMode} Stream Live` : 'Reconnecting...'}</span>
          </div>
          {lastSync && (
            <span className="text-xs text-muted font-mono">
              Updated {lastSync.toLocaleTimeString()}
            </span>
          )}
        </div>
      </header>

      {/* Dual-Engine Mode Switcher & Independent Bot Execution Control Bar */}
      <div className={`mode-control-banner mb-lg glass-panel ${viewMode === 'PAPER' ? 'paper-theme' : 'live-theme'}`}>
        <div className="mode-control-left">
          <div className="mode-segmented-tabs">
            <button
              type="button"
              className={`mode-seg-btn paper ${viewMode === 'PAPER' ? 'active' : ''}`}
              onClick={() => switchViewMode('PAPER')}
            >
              <FlaskConical size={16} />
              <span>🧪 모의매매 뷰 (Paper)</span>
              <span className={`bot-mini-pill ${(portfolio.paperBotStatus || 'RUNNING').toLowerCase()}`}>
                {portfolio.paperBotStatus || 'RUNNING'}
              </span>
            </button>
            <button
              type="button"
              className={`mode-seg-btn live ${viewMode === 'LIVE' ? 'active' : ''}`}
              onClick={() => switchViewMode('LIVE')}
            >
              <Zap size={16} />
              <span>⚡ 실매매 뷰 (Live)</span>
              <span className={`bot-mini-pill ${(portfolio.liveBotStatus || 'STOPPED').toLowerCase()}`}>
                {portfolio.liveBotStatus || 'STOPPED'}
              </span>
            </button>
          </div>

          <div className="mode-control-desc">
            {viewMode === 'PAPER' ? (
              <span>
                <strong>100% 실제 거래소 실시간 호가(Upbit/Coinbase)</strong> 기반 가상 시드 매매 중입니다. 뷰를 전환해도 백그라운드 매매는 중단되지 않습니다.
              </span>
            ) : (
              <span>
                <strong>실제 거래소 API 키 및 실자본</strong>으로 델타 중립 아비트라지를 수행하는 실전 엔진입니다.
              </span>
            )}
          </div>
        </div>

        <div className="mode-control-right">
          <div className="bot-status-indicator">
            <span className="text-xs text-muted">{viewMode} 자동매매 상태:</span>
            <span className={`bot-status-badge ${currentBotStatus.toLowerCase()}`}>
              {currentBotStatus === 'RUNNING' && '● RUNNING (자동 진입·청산 활성)'}
              {currentBotStatus === 'DRAINING' && '◐ DRAINING (기존 포지션만 청산 대기)'}
              {currentBotStatus === 'STOPPED' && '○ STOPPED (일시 정지)'}
            </span>
          </div>

          <div className="bot-action-buttons">
            {currentBotStatus === 'RUNNING' ? (
              <button
                type="button"
                disabled={statusUpdating}
                className="btn-bot-stop"
                onClick={handleStopButtonClick}
              >
                <Pause size={14} /> 자동매매 중지
              </button>
            ) : (
              <>
                <button
                  type="button"
                  disabled={statusUpdating}
                  className="btn-bot-start"
                  onClick={() => executeBotStatusChange('RUN')}
                >
                  <Play size={14} /> {currentBotStatus === 'DRAINING' ? '신규 진입 재개 (Resume)' : '자동매매 시작 (Start)'}
                </button>
                {currentBotStatus === 'DRAINING' && activePositions.length > 0 && (
                  <button
                    type="button"
                    disabled={statusUpdating}
                    className="btn-bot-stop"
                    onClick={() => executeBotStatusChange('STOP_AND_CLOSE')}
                  >
                    전체 즉시 청산 후 정지
                  </button>
                )}
              </>
            )}
          </div>
        </div>
      </div>

      {/* Paper Mode Exclusive: Web-Controlled Gradual Price Offset Simulator (±N% over X minutes) */}
      {viewMode === 'PAPER' && (
        <div className="sim-offset-card glass-panel mb-lg">
          <div className="flex-between mb-sm flex-wrap gap-sm">
            <div className="flex items-center gap-sm">
              <Sliders size={19} className="text-emerald-400" />
              <h2 className="text-base font-bold m-0">
                실시간 거래소 가격 변동 시뮬레이터 (±N% Gradual Offset Simulator)
              </h2>
              <span className="sim-live-pill">
                현재 적용: {simStatus.exchange} {simStatus.current_offset_pct >= 0 ? '+' : ''}{(simStatus.current_offset_pct || 0).toFixed(2)}%
              </span>
            </div>

            <div className="sim-preset-group">
              <span className="text-xs text-muted mr-1">1-Click 시나리오:</span>
              <button
                type="button"
                disabled={simSubmitting}
                className="sim-preset-btn widen"
                onClick={() => applySimOffset('UPBIT', 5.0, 3)}
              >
                📈 김프 확대 (+5.0% / 3분)
              </button>
              <button
                type="button"
                disabled={simSubmitting}
                className="sim-preset-btn converge"
                onClick={() => applySimOffset('UPBIT', 0.0, 3)}
              >
                📉 김프 수렴 익절 (0.0% / 3분)
              </button>
              <button
                type="button"
                disabled={simSubmitting}
                className="sim-preset-btn reverse"
                onClick={() => applySimOffset('UPBIT', -1.5, 2)}
              >
                🔄 역프 리밸런싱 (-1.5% / 2분)
              </button>
              <button
                type="button"
                disabled={simSubmitting}
                className="sim-preset-btn reset"
                onClick={() => applySimOffset(simStatus.exchange || 'UPBIT', 0.0, 0)}
              >
                <RotateCcw size={12} /> 즉시 0% 복원
              </button>
            </div>
          </div>

          <p className="text-xs text-secondary mb-md">
            실제 업비트/코인베이스 실시간 시세에 선택한 거래소 가격만 <strong>X분 동안 코사인 S-커브로 부드럽게 ±N% 가감</strong>하여, 프리미엄 확대 진입·수렴 익절·역프 100% 델타 중립 자동 리밸런싱이 정상 작동하는지 검증합니다.
          </p>

          {/* Live Ramp Progress Bar */}
          {(simStatus.is_ramping || Math.abs(simStatus.current_offset_pct || 0) > 0.001) && (
            <div className="sim-progress-box mb-md">
              <div className="flex-between text-xs font-mono mb-xs">
                <span>
                  대상 거래소: <strong className="text-emerald-300">{simStatus.exchange}</strong> | 시작: {(simStatus.start_offset_pct || 0).toFixed(2)}% → 현재: <strong className="text-white">{simStatus.current_offset_pct >= 0 ? '+' : ''}{(simStatus.current_offset_pct || 0).toFixed(2)}%</strong> → 목표: <strong className="text-amber-300">{simStatus.target_offset_pct >= 0 ? '+' : ''}{(simStatus.target_offset_pct || 0).toFixed(2)}%</strong>
                </span>
                <span>
                  {simStatus.is_ramping
                    ? `진행률 ${(simStatus.progress_pct || 0).toFixed(0)}% (남은 시간: ${formatRemainingTime(simStatus.remaining_seconds)})`
                    : '목표 오프셋 도달 완료 (유지 중)'}
                </span>
              </div>
              <div className="sim-progress-track">
                <div
                  className="sim-progress-fill"
                  style={{ width: `${Math.max(0, Math.min(100, simStatus.progress_pct || 100))}%` }}
                />
              </div>
            </div>
          )}

          {/* Custom Simulator Form */}
          <form
            className="sim-controls-grid"
            onSubmit={(e) => {
              e.preventDefault();
              applySimOffset(simForm.exchange, simForm.targetOffsetPct, simForm.rampMinutes);
            }}
          >
            <div className="sim-control-item">
              <label className="text-xs text-secondary font-semibold">대상 거래소 (Target Exchange)</label>
              <select
                value={simForm.exchange}
                onChange={(e) => setSimForm({ ...simForm, exchange: e.target.value })}
                className="sim-input"
              >
                <option value="UPBIT">UPBIT (업비트 KRW 시세 가감)</option>
                <option value="COINBASE">COINBASE (코인베이스 USD 시세 가감)</option>
              </select>
            </div>

            <div className="sim-control-item">
              <label className="text-xs text-secondary font-semibold">
                목표 가격 오프셋 (±N%): <strong className="text-amber-300 font-mono">{simForm.targetOffsetPct >= 0 ? '+' : ''}{simForm.targetOffsetPct}%</strong>
              </label>
              <div className="flex items-center gap-sm">
                <input
                  type="range"
                  min="-10.0"
                  max="15.0"
                  step="0.5"
                  value={simForm.targetOffsetPct}
                  onChange={(e) => setSimForm({ ...simForm, targetOffsetPct: parseFloat(e.target.value) })}
                  className="flex-1"
                />
                <input
                  type="number"
                  step="0.1"
                  min="-20"
                  max="30"
                  value={simForm.targetOffsetPct}
                  onChange={(e) => setSimForm({ ...simForm, targetOffsetPct: e.target.value })}
                  className="sim-input sim-num-input font-mono"
                />
              </div>
            </div>

            <div className="sim-control-item">
              <label className="text-xs text-secondary font-semibold">
                점진 변동 소요 시간 (X분, 0=즉시)
              </label>
              <input
                type="number"
                step="0.5"
                min="0"
                max="60"
                value={simForm.rampMinutes}
                onChange={(e) => setSimForm({ ...simForm, rampMinutes: e.target.value })}
                className="sim-input font-mono"
              />
            </div>

            <div className="sim-control-item sim-control-actions">
              <button type="submit" disabled={simSubmitting} className="btn-sim-apply">
                {simSubmitting ? '적용 중...' : '시뮬레이션 오프셋 적용'}
              </button>
            </div>
          </form>
        </div>
      )}

      {/* Critical Web Lending Session Banner */}
      {disconnectedSessions.length > 0 && viewMode === 'LIVE' && (
        <div className="lending-alert-banner mb-lg animate-fade-in">
          <div className="flex items-center gap-md">
            <ShieldAlert size={22} className="text-red-400 flex-shrink-0" />
            <div>
              <div className="font-bold text-sm text-red-300">
                Web Coin Lending Session Disconnected: {disconnectedSessions.join(', ')}
              </div>
              <div className="text-xs text-red-200/80 mt-xs">
                New Margin Short entries on {disconnectedSessions.join(', ')} are automatically paused by the Engine Watchdog. Re-authenticate via noVNC QR/2FA to resume.
              </div>
            </div>
          </div>
          <button
            className="lending-alert-btn"
            onClick={() => navigate('/settings?tab=exchange&openNoVNC=1')}
          >
            Open noVNC Login <ExternalLink size={14} />
          </button>
        </div>
      )}

      <div className="dashboard-grid">
        {/* Left Column: Portfolio & Capital Skew + Active Positions Lifecycle */}
        <div className="dashboard-col-left flex flex-col gap-lg">
          {/* Portfolio & Capital Skew Gauge */}
          <div className="dashboard-card glass-panel">
            <div className="flex-between mb-md flex-wrap gap-sm">
              <h2 className="text-lg font-semibold flex items-center gap-sm m-0">
                <PieChart size={20} className="text-blue-400" />
                {viewMode === 'PAPER' ? 'Virtual Capital & Delta-Neutral Skew' : 'Live Capital & Delta-Neutral Skew'}
              </h2>
              <div className="flex items-center gap-sm">
                {viewMode === 'PAPER' && (
                  <button
                    type="button"
                    className="btn-reset-seed"
                    onClick={handleResetPaperSeed}
                    title="Reset virtual KRW/USD balances and close open paper positions"
                  >
                    <RotateCcw size={12} /> 가상 시드 초기화
                  </button>
                )}
                <span className={`skew-status-badge ${isSkewCritical ? 'critical' : isSkewWarning ? 'warning' : 'healthy'}`}>
                  <Scale size={13} />
                  {isSkewCritical
                    ? 'Auto-Rebalance Triggered'
                    : isSkewWarning
                      ? 'Approaching Skew Limit'
                      : 'Balanced (50:50 Target)'}
                </span>
              </div>
            </div>

            <div className="portfolio-metrics-row mb-lg">
              <div className="portfolio-metric-box">
                <span className="text-xs text-muted uppercase tracking-wider">
                  {viewMode === 'PAPER' ? 'Virtual Portfolio Equity (USD)' : 'Total Portfolio Equity (USD)'}
                </span>
                <div className="text-3xl font-bold font-mono mt-xs">
                  ${portfolio.totalUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
                </div>
              </div>
              <div className="portfolio-metric-box">
                <span className="text-xs text-muted uppercase tracking-wider">Available Free Quote</span>
                <div className="text-2xl font-bold font-mono text-green-400 mt-xs">
                  ${portfolio.availableUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
                </div>
              </div>
            </div>

            {/* Visual Capital Skew Gauge Bar */}
            <div className="skew-gauge-section mb-lg">
              <div className="flex-between text-xs mb-xs">
                <span className="text-secondary font-semibold">
                  Foreign USD Capital Ratio: <strong className="text-white font-mono">{foreignSharePct.toFixed(1)}%</strong>
                </span>
                <span className="text-muted font-mono">
                  Trigger ≤ {triggerPct.toFixed(0)}% | Target {targetPct.toFixed(0)}%
                </span>
              </div>

              <div className="skew-bar-track">
                <div
                  className={`skew-bar-fill ${isSkewCritical ? 'critical' : isSkewWarning ? 'warning' : 'healthy'}`}
                  style={{ width: `${foreignSharePct}%` }}
                />
                <div
                  className="skew-marker trigger"
                  style={{ left: `${triggerPct}%` }}
                  title={`Auto-Rebalance Trigger (${triggerPct.toFixed(0)}%)`}
                >
                  <span className="skew-marker-label">{triggerPct.toFixed(0)}% Min</span>
                </div>
                <div
                  className="skew-marker target"
                  style={{ left: `${targetPct}%` }}
                  title={`Target Balance (${targetPct.toFixed(0)}%)`}
                >
                  <span className="skew-marker-label">{targetPct.toFixed(0)}% Target</span>
                </div>
              </div>
            </div>

            {/* Per-Exchange Breakdown Cards */}
            <div className="exchange-balances-grid">
              {portfolio.exchanges.map((ex) => {
                const sessionValid = portfolio.webLendingSessions?.[ex.name] ?? ex.webLendingOk;
                return (
                  <div key={ex.name} className="exchange-balance-card">
                    <div className="flex-between mb-xs">
                      <span className="font-bold text-sm flex items-center gap-xs">
                        {ex.name}
                        {typeof sessionValid === 'boolean' && (
                          <span
                            className={`lending-dot ${sessionValid ? 'ok' : 'lost'}`}
                            title={sessionValid ? 'Web Lending Session Active' : 'Web Lending Session Disconnected'}
                          />
                        )}
                      </span>
                      <span className="text-xs font-mono px-2 py-0.5 rounded bg-blue-500/10 text-blue-300 border border-blue-500/20">
                        {(ex.percent || 0).toFixed(1)}%
                      </span>
                    </div>
                    <div className="text-lg font-bold font-mono">
                      ${(ex.usdValue || 0).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
                    </div>
                    {ex.nativeBalance > 0 && ex.asset === 'KRW' && (
                      <div className="text-xs font-mono text-secondary mt-0.5">
                        ₩{Math.round(ex.nativeBalance).toLocaleString()} KRW
                      </div>
                    )}
                    <div className="flex-between mt-xs text-xs text-muted">
                      <span>Quote Asset: {ex.asset || 'USD'}</span>
                      {typeof sessionValid === 'boolean' && (
                        <span className={sessionValid ? 'text-green-400' : 'text-red-400'}>
                          {viewMode === 'PAPER' ? 'Virtual Lending OK' : (sessionValid ? 'CDP Lending Ready' : 'CDP Auth Needed')}
                        </span>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>

          {/* Active Positions Lifecycle Pipeline */}
          <div className="dashboard-card glass-panel">
            <div className="flex-between mb-md">
              <h2 className="text-lg font-semibold flex items-center gap-sm m-0">
                <TrendingUp size={20} className="text-green-400" />
                Active {viewMode === 'PAPER' ? 'Paper' : 'Live'} Positions ({activePositions.length})
              </h2>
            </div>

            {activePositions.length === 0 ? (
              <div className="empty-state-box">
                No open {viewMode.toLowerCase()} positions currently held. The {viewMode} Engine is scanning for delta-neutral entry signals.
              </div>
            ) : (
              <div className="flex flex-col gap-md">
                {activePositions.map((pos) => {
                  const stage = getLifecycleStage(pos.status);
                  const isProfit = pos.currentPnlUsd >= 0;

                  return (
                    <div key={pos.id} className="position-lifecycle-card">
                      <div className="flex-between mb-sm">
                        <div className="flex items-center gap-sm">
                          <span className="strategy-pill">{pos.strategy}</span>
                          <span className="font-bold text-sm">{pos.pair}</span>
                        </div>
                        <span className="text-xs text-muted font-mono">{pos.id}</span>
                      </div>

                      <div className="lifecycle-steps my-md">
                        <div className={`lifecycle-step ${stage >= 1 ? 'active' : ''}`}>
                          <CheckCircle2 size={14} />
                          <span>1. OPEN (Hedged)</span>
                        </div>
                        <div className="lifecycle-connector" />
                        <div className={`lifecycle-step ${stage >= 2 ? 'active' : ''}`}>
                          <CheckCircle2 size={14} />
                          <span>2. TRANSFERRING</span>
                        </div>
                        <div className="lifecycle-connector" />
                        <div className={`lifecycle-step ${stage >= 3 ? 'active' : ''}`}>
                          <CheckCircle2 size={14} />
                          <span>3. AWAITING REPAY</span>
                        </div>
                      </div>

                      <div className="position-metrics-grid">
                        <div>
                          <div className="text-xs text-muted">Invested Size</div>
                          <div className="font-mono font-semibold text-sm mt-xs">
                            ${pos.investedUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
                          </div>
                        </div>
                        <div>
                          <div className="text-xs text-muted">Entry Spread / Target Spread</div>
                          <div className="font-mono text-sm mt-xs text-secondary">
                            {pos.entrySpreadPct.toFixed(2)}% / <span className="text-amber-400">{pos.expectedRoiPct.toFixed(2)}%</span>
                          </div>
                        </div>
                        <div className="text-right">
                          <div className="text-xs text-muted">Live Unrealized PnL</div>
                          <div className={`font-mono font-bold text-sm mt-xs ${isProfit ? 'text-green-400' : 'text-red-400'}`}>
                            {isProfit ? '+' : ''}${pos.currentPnlUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })} ({isProfit ? '+' : ''}{pos.currentPnlPct.toFixed(2)}%)
                          </div>
                        </div>
                      </div>

                      <div className="mt-md pt-sm border-t border-white/10 flex-between">
                        <span className="text-xs text-muted">
                          Route: {pos.baseExchange} (Short) → {pos.foreignExchange} (Long)
                        </span>
                        <button
                          className="btn-force-close"
                          onClick={() => handleForceClose(pos)}
                        >
                          <AlertOctagon size={13} /> Force Close & Repay
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        </div>

        {/* Right Column: Top AI Actionable Signals & Live Engine Telemetry */}
        <div className="dashboard-col-right flex flex-col gap-lg">
          {/* Top AI Recommended Opportunities */}
          <div className="dashboard-card glass-panel">
            <div className="flex-between mb-md">
              <h2 className="text-lg font-semibold flex items-center gap-sm m-0">
                <Zap size={20} className="text-amber-400" /> Top AI Actionable Signals ({viewMode})
              </h2>
              <button
                className="link-scanner-btn"
                onClick={() => navigate('/opportunities')}
              >
                Full Global Scanner <ArrowRight size={14} />
              </button>
            </div>

            {topOpportunities.length === 0 ? (
              <div className="empty-state-box">
                Waiting for AI Forecaster & RL Portfolio Agent signals matching your risk thresholds...
              </div>
            ) : (
              <div className="flex flex-col gap-sm">
                {topOpportunities.map((o, idx) => {
                  const asset = o.base_symbol?.split('-')[0] || o.base_symbol;
                  const currPrem = typeof o.current_premium_pct === 'number'
                    ? o.current_premium_pct
                    : (typeof o.current_prem === 'number' ? o.current_prem * 100 : 0);
                  const predPrem = typeof o.predicted_premium_pct === 'number'
                    ? o.predicted_premium_pct
                    : (typeof o.expected_future_prem === 'number' ? o.expected_future_prem * 100 : currPrem);
                  const weightPct = ((o.target_weight || 0) * 100).toFixed(1);
                  const roiPct = ((o.expected_roi || 0) * 100).toFixed(2);

                  return (
                    <div key={idx} className="signal-compact-card">
                      <div className="flex-between mb-xs">
                        <div className="flex items-center gap-sm">
                          <span className="asset-badge">{asset}</span>
                          <div className="route-container">
                            <span className="exchange-name base">{o.base_exchange}</span>
                            <span className="route-arrow">→</span>
                            <span className="exchange-name foreign">{o.foreign_exchange}</span>
                          </div>
                          <span className="text-xs text-secondary font-semibold">{o.strategy}</span>
                        </div>
                        <span className="roi-value positive">+{roiPct}% Net ROI</span>
                      </div>

                      <div className="signal-metrics-row">
                        <span>
                          Spread: <strong className="font-mono text-white">{currPrem.toFixed(2)}%</strong> → 8h Pred: <strong className="font-mono text-amber-300">{predPrem.toFixed(2)}%</strong>
                        </span>
                        <span>
                          AI Weight: <strong className="font-mono text-blue-300">{weightPct}%</strong>
                        </span>
                        <span>
                          Risk: <strong className={`font-mono ${o.risk_score > 30 ? 'text-red-400' : 'text-green-400'}`}>{o.risk_score?.toFixed(1)}</strong>
                        </span>
                        <button
                          className="btn-enter"
                          onClick={() => handleForceEnter(o)}
                        >
                          Force Enter ({viewMode})
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>

          {/* Real-Time Engine Event Log */}
          <div className="dashboard-card glass-panel">
            <div className="flex-between mb-md">
              <h2 className="text-lg font-semibold flex items-center gap-sm m-0">
                <Terminal size={19} className="text-purple-400" /> Live Engine Telemetry & Alerts
              </h2>
              {eventLogs.length > 0 && (
                <button
                  className="text-xs text-muted hover:text-white"
                  style={{ background: 'transparent', border: 'none', cursor: 'pointer' }}
                  onClick={() => setEventLogs([])}
                >
                  Clear
                </button>
              )}
            </div>

            {eventLogs.length === 0 ? (
              <div className="empty-state-box">
                Listening to Redis pub/sub events (`user:notifications`, `user:cmd`, `user:state`)...
              </div>
            ) : (
              <div className="engine-log-feed">
                {eventLogs.map((log) => (
                  <div key={log.id} className={`engine-log-item ${log.level}`}>
                    <div className="flex-between text-xs mb-1">
                      <span className="font-mono font-bold">{log.type}</span>
                      <span className="font-mono text-muted">{log.time}</span>
                    </div>
                    <div className="text-xs text-secondary">{log.message}</div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>

      {/* 3-Option Confirmation Modal when Stopping Bot with Open Positions */}
      {showStopModal && (
        <div className="modal-backdrop">
          <div className="stop-modal-card glass-panel animate-fade-in">
            <h3 className="text-lg font-bold flex items-center gap-sm text-amber-300 m-0">
              <AlertOctagon size={20} /> {viewMode === 'PAPER' ? '모의매매(Paper)' : '실매매(Live)'} 자동매매 중지 옵션 선택
            </h3>
            <p className="text-sm text-secondary mt-sm mb-lg">
              현재 <strong>{activePositions.length}개의 열린 {viewMode === 'PAPER' ? '모의' : '실전'} 포지션</strong>이 운용 중입니다. 열린 포지션을 어떻게 처리할지 선택해 주세요.
            </p>

            <div className="flex flex-col gap-sm">
              <button
                type="button"
                disabled={statusUpdating}
                className="stop-option-btn drain"
                onClick={() => executeBotStatusChange('DRAIN')}
              >
                <div className="font-bold text-sm text-amber-300">
                  1. 기존 포지션만 청산 시까지 유지 (Drain Mode — 권장)
                </div>
                <div className="text-xs text-secondary mt-1">
                  신규 포지션 진입만 즉시 차단하고, 이미 열려 있는 {activePositions.length}개 포지션은 목표 익절/손절 조건 도달 시까지 안전하게 모니터링 후 자동 청산합니다.
                </div>
              </button>

              <button
                type="button"
                disabled={statusUpdating}
                className="stop-option-btn close-all"
                onClick={() => executeBotStatusChange('STOP_AND_CLOSE')}
              >
                <div className="font-bold text-sm text-red-300">
                  2. 모든 열린 포지션 즉시 시장가 청산 후 중지 (Force Close All)
                </div>
                <div className="text-xs text-secondary mt-1">
                  열려 있는 {activePositions.length}개 포지션을 즉시 델타 중립 시장가로 청산 및 대여 상환하고 엔진을 완전히 정지(STOPPED)합니다.
                </div>
              </button>

              <button
                type="button"
                disabled={statusUpdating}
                className="stop-option-btn cancel"
                onClick={() => setShowStopModal(false)}
              >
                3. 취소 (계속 자동매매 유지)
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};
