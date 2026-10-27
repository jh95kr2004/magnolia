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
  ExternalLink
} from 'lucide-react';
import { apiClient } from '../../services/api';
import { buildSseUrl } from '../../contexts/AuthContext';
import { useToast } from '../../contexts/ToastContext';
import './Dashboard.css';

export const Dashboard = () => {
  const navigate = useNavigate();
  const { showToast, requestConfirm } = useToast();

  const [sseConnected, setSseConnected] = useState(false);
  const [lastSync, setLastSync] = useState(null);

  const [portfolio, setPortfolio] = useState({
    totalUsd: 0,
    availableUsd: 0,
    foreignShare: 0.5,
    rebalanceTriggerSkew: 0.2,
    rebalanceTargetSkew: 0.5,
    webLendingSessions: {},
    exchanges: []
  });

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
    const dashboardSrc = new EventSource(buildSseUrl('/api/ws/dashboard'), { withCredentials: true });
    const oppsSrc = new EventSource(buildSseUrl('/api/ws/opportunities'), { withCredentials: true });

    dashboardSrc.onopen = () => setSseConnected(true);
    dashboardSrc.onerror = () => setSseConnected(false);

    dashboardSrc.addEventListener('portfolio', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data) {
          setPortfolio({
            totalUsd: typeof data.total_equity_usd === 'number' ? data.total_equity_usd : 0,
            availableUsd: typeof data.available_usd === 'number' ? data.available_usd : 0,
            foreignShare: typeof data.foreign_share === 'number' ? data.foreign_share : 0.5,
            rebalanceTriggerSkew: data.rebalance_trigger_skew || 0.2,
            rebalanceTargetSkew: data.rebalance_target_skew || 0.5,
            webLendingSessions: data.web_lending_sessions || {},
            exchanges: Array.isArray(data.portfolio) ? data.portfolio : []
          });
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
              strategy: p.Type,
              baseExchange: p.BaseExchange,
              foreignExchange: p.ForeignExchange,
              pair: `${p.BaseSymbol} → ${p.ForeignSymbol}`,
              investedUsd: p.InvestmentUSD || 0,
              entrySpreadPct: (p.EntrySpread || 0) * 100,
              expectedRoiPct: (p.ExpectedROI || 0) * 100,
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
          } else if (t.includes('OPENED') || t.includes('CLOSED') || t.includes('COMPLETED')) {
            toastType = 'success';
          } else if (t.includes('QUEUED') || t.includes('REBALANCE')) {
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
  }, [showToast]);

  const handleForceClose = (pos) => {
    requestConfirm({
      title: `Force Close Position (${pos.id})`,
      message: `Are you sure you want to immediately unwind ${pos.pair} (${pos.strategy})?\n\nThe Go Engine will execute delta-neutral market orders on ${pos.baseExchange} & ${pos.foreignExchange} and repay any active coin loans.`,
      confirmText: 'Execute Market Unwind',
      danger: true,
      onConfirm: async () => {
        try {
          await apiClient.delete(`/positions/active/${pos.id}`);
          showToast(`Force close command dispatched to Engine for ${pos.id}.`, 'warning');
          appendEventLog('FORCE_CLOSE_SENT', `Requested immediate unwind for ${pos.id}`, 'warning');
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
      title: `Force Enter Arbitrage (${asset})`,
      message: `Route: ${route}\nStrategy: ${opp.strategy}\nExpected Net ROI: +${roiPct}%\nSuggested Allocation Weight: ${weightPct}%\n\nThis sends a real execution command to your user Engine goroutine.`,
      confirmText: 'Execute Hedge Entry',
      danger: false,
      onConfirm: async () => {
        try {
          await apiClient.post('/positions/force_enter', opp);
          showToast(`Force entry command sent to Engine for ${route}.`, 'success');
          appendEventLog('FORCE_ENTER_SENT', `Dispatched ${opp.strategy} entry on ${route}`, 'success');
        } catch (err) {
          showToast('Failed to dispatch entry: ' + (err.response?.data?.error || err.message), 'error');
        }
      }
    });
  };

  // Detect any disconnected web lending sessions
  const disconnectedSessions = Object.entries(portfolio.webLendingSessions || {})
    .filter(([, valid]) => valid === false)
    .map(([ex]) => ex);

  const foreignSharePct = Math.max(0, Math.min(100, (portfolio.foreignShare || 0) * 100));
  const triggerPct = (portfolio.rebalanceTriggerSkew || 0.2) * 100;
  const targetPct = (portfolio.rebalanceTargetSkew || 0.5) * 100;

  const isSkewCritical = foreignSharePct <= triggerPct;
  const isSkewWarning = !isSkewCritical && foreignSharePct <= triggerPct + 8;

  const getLifecycleStage = (status) => {
    const s = (status || 'OPEN').toUpperCase();
    if (s.includes('REPAY')) return 3;
    if (s.includes('TRANSFER')) return 2;
    return 1;
  };

  return (
    <div className="dashboard-page animate-fade-in delay-100">
      {/* Header */}
      <header className="page-header mb-lg flex-between">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-sm">
            <Activity className="text-accent-primary" /> Arbitrage Control Center
          </h1>
          <p className="text-secondary mt-xs">
            Real-time delta-neutral capital skew, active position lifecycle, and AI execution telemetry.
          </p>
        </div>
        <div className="flex items-center gap-md">
          <div className={`sse-status-pill ${sseConnected ? 'online' : 'offline'}`}>
            <Radio size={13} className={sseConnected ? 'animate-pulse' : ''} />
            <span>{sseConnected ? 'Engine Stream Live' : 'Reconnecting...'}</span>
          </div>
          {lastSync && (
            <span className="text-xs text-muted font-mono">
              Updated {lastSync.toLocaleTimeString()}
            </span>
          )}
        </div>
      </header>

      {/* Critical Web Lending Session Banner */}
      {disconnectedSessions.length > 0 && (
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
            <div className="flex-between mb-md">
              <h2 className="text-lg font-semibold flex items-center gap-sm m-0">
                <PieChart size={20} className="text-blue-400" /> Capital & Delta-Neutral Skew
              </h2>
              <span className={`skew-status-badge ${isSkewCritical ? 'critical' : isSkewWarning ? 'warning' : 'healthy'}`}>
                <Scale size={13} />
                {isSkewCritical
                  ? 'Auto-Rebalance Triggered'
                  : isSkewWarning
                    ? 'Approaching Skew Limit'
                    : 'Balanced (50:50 Target)'}
              </span>
            </div>

            <div className="portfolio-metrics-row mb-lg">
              <div className="portfolio-metric-box">
                <span className="text-xs text-muted uppercase tracking-wider">Total Portfolio Equity</span>
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
                {/* Trigger Marker Line (20%) */}
                <div
                  className="skew-marker trigger"
                  style={{ left: `${triggerPct}%` }}
                  title={`Auto-Rebalance Trigger (${triggerPct.toFixed(0)}%)`}
                >
                  <span className="skew-marker-label">{triggerPct.toFixed(0)}% Min</span>
                </div>
                {/* Target Marker Line (50%) */}
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
                const sessionValid = portfolio.webLendingSessions?.[ex.name];
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
                    <div className="flex-between mt-xs text-xs text-muted">
                      <span>Quote Asset: {ex.asset || 'USD'}</span>
                      {typeof sessionValid === 'boolean' && (
                        <span className={sessionValid ? 'text-green-400' : 'text-red-400'}>
                          {sessionValid ? 'CDP Lending Ready' : 'CDP Auth Needed'}
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
                <TrendingUp size={20} className="text-green-400" /> Active Hedged Positions ({activePositions.length})
              </h2>
            </div>

            {activePositions.length === 0 ? (
              <div className="empty-state-box">
                No open positions currently held. The AI Engine is continuously scanning for delta-neutral entry signals.
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

                      {/* 3-Step Visual Lifecycle Pipeline */}
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
                          <div className="text-xs text-muted">Entry Spread / Target ROI</div>
                          <div className="font-mono text-sm mt-xs text-secondary">
                            {pos.entrySpreadPct.toFixed(2)}% / <span className="text-amber-400">+{pos.expectedRoiPct.toFixed(2)}%</span>
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
                <Zap size={20} className="text-amber-400" /> Top AI Actionable Signals
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
                  const currPrem = typeof o.current_premium_pct === 'number' ? o.current_premium_pct : 0;
                  const predPrem = typeof o.predicted_premium_pct === 'number' ? o.predicted_premium_pct : currPrem;
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
                          Force Enter
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
    </div>
  );
};
