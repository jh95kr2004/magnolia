import React, { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { ArrowRight, ExternalLink, AlertTriangle } from 'lucide-react';
import { apiClient } from '../../services/api';
import { buildSseUrl, useAuth } from '../../contexts/AuthContext';
import { useI18n } from '../../contexts/LanguageContext';
import { formatStrategyLabel, formatPositionStatus } from '../../i18n/translations';
import { useToast } from '../../contexts/ToastContext';

export const Dashboard = () => {
  const navigate = useNavigate();
  const { viewMode, setBotStatuses } = useAuth();
  const { lang, t } = useI18n();
  const { showToast, requestConfirm } = useToast();

  const [sseConnected, setSseConnected] = useState(false);
  const [portfolio, setPortfolio] = useState({
    totalUsd: 0,
    availableUsd: 0,
    foreignShare: 0.5,
    rebalanceTriggerSkew: 0.2,
    webLendingSessions: {},
    exchanges: []
  });
  const [activePositions, setActivePositions] = useState([]);

  const mapPositions = (rawList) => {
    if (!Array.isArray(rawList)) return [];
    return rawList.map((p) => ({
      id: p.PositionID || p.ID,
      strategy: p.Type,
      baseExchange: p.BaseExchange,
      foreignExchange: p.ForeignExchange,
      baseSymbol: p.BaseSymbol || 'BTC-KRW',
      foreignSymbol: p.ForeignSymbol || 'BTC-USD',
      investedUsd: Number(p.InvestmentUSD || 0),
      entrySpreadPct: Number((p.EntrySpread ?? p.EntryPremium) || 0) * 100,
      currentPnlUsd:
        typeof p.CurrentPnLUSD === 'number'
          ? p.CurrentPnLUSD
          : Number(p.RealizedProfitUSD || 0),
      currentPnlPct: typeof p.CurrentPnLPct === 'number' ? p.CurrentPnLPct : 0,
      status: p.Status || 'OPEN',
      openedAt: p.OpenedAt
    }));
  };

  useEffect(() => {
    setSseConnected(false);

    // Initial REST fetch
    apiClient
      .get(`/positions/active?mode=${viewMode}`)
      .then((res) => setActivePositions(mapPositions(res.data)))
      .catch(() => {});

    const dashboardSrc = new EventSource(buildSseUrl('/api/ws/dashboard', viewMode), {
      withCredentials: true
    });

    dashboardSrc.onopen = () => setSseConnected(true);
    dashboardSrc.onerror = () => setSseConnected(false);

    dashboardSrc.addEventListener('portfolio', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (!data) return;

        const rawEx = Array.isArray(data.exchange_balances)
          ? data.exchange_balances
          : Array.isArray(data.portfolio)
            ? data.portfolio
            : [];

        const mappedEx = rawEx.map((ex) => ({
          name: String(ex.name || '').toUpperCase(),
          quote: ex.quote_currency || (String(ex.name || '').toUpperCase() === 'UPBIT' ? 'KRW' : 'USD'),
          nativeBalance: Number(ex.available_native || 0),
          usdValue: Number(ex.available_usd ?? ex.usdValue ?? 0),
          percent: Number(ex.percent || 0),
          webLendingOk: ex.web_lending_ok
        }));

        setPortfolio({
          totalUsd: Number(data.total_equity_usd || 0),
          availableUsd: Number(data.available_balance ?? data.available_usd ?? 0),
          foreignShare: typeof data.foreign_share === 'number' ? data.foreign_share : 0.5,
          rebalanceTriggerSkew: Number(data.rebalance_trigger_skew || 0.2),
          webLendingSessions: data.web_lending_sessions || {},
          exchanges: mappedEx
        });

        if (data.paper_bot_status || data.live_bot_status) {
          setBotStatuses({
            PAPER: data.paper_bot_status || 'RUNNING',
            LIVE: data.live_bot_status || 'STOPPED'
          });
        }
        setSseConnected(true);
      } catch {}
    });

    dashboardSrc.addEventListener('positions', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data && Array.isArray(data.positions)) {
          setActivePositions(mapPositions(data.positions));
        }
      } catch {}
    });

    return () => {
      dashboardSrc.close();
    };
  }, [viewMode]);

  const handleClosePosition = (pos) => {
    requestConfirm({
      title: t.dashboard.confirmCloseTitle,
      message: `${pos.baseSymbol} (${pos.baseExchange} ↔ ${pos.foreignExchange})\n${t.dashboard.confirmCloseDesc}`,
      confirmText: t.dashboard.closePositionBtn,
      cancelText: t.stopModal.cancel,
      danger: true,
      onConfirm: async () => {
        try {
          await apiClient.delete(`/positions/active/${pos.id}?mode=${viewMode}`);
          showToast(
            lang === 'ko'
              ? '포지션 청산 명령이 전송되었습니다.'
              : 'Position close command dispatched.',
            'success'
          );
          const res = await apiClient.get(`/positions/active?mode=${viewMode}`);
          setActivePositions(mapPositions(res.data));
        } catch (err) {
          showToast(err.response?.data?.error || err.message, 'error');
        }
      }
    });
  };

  const displayExchanges =
    portfolio.exchanges && portfolio.exchanges.length > 0
      ? portfolio.exchanges
      : [
          {
            name: 'UPBIT',
            quoteCurrency: 'KRW',
            nativeBalance: viewMode === 'PAPER' ? 50000000 : 0,
            usdValue: viewMode === 'PAPER' ? 36000 : 0,
            percent: 50
          },
          {
            name: 'COINBASE',
            quoteCurrency: 'USD',
            nativeBalance: viewMode === 'PAPER' ? 35000 : 0,
            usdValue: viewMode === 'PAPER' ? 35000 : 0,
            percent: 50
          }
        ];

  const sumExchangeUsd = displayExchanges.reduce((acc, ex) => acc + (ex.usdValue || 0), 0);
  const totalUnrealizedPnl = activePositions.reduce((acc, p) => acc + (p.currentPnlUsd || 0), 0);
  const displayTotalUsd =
    portfolio.totalUsd > 0 ? portfolio.totalUsd : sumExchangeUsd + totalUnrealizedPnl;

  const foreignPct = Math.round((portfolio.foreignShare || 0.5) * 100);
  const domesticPct = Math.max(0, 100 - foreignPct);
  const isSkewLow = (portfolio.foreignShare || 0.5) <= (portfolio.rebalanceTriggerSkew || 0.2);

  const isUpbitLendingDisconnected =
    viewMode === 'LIVE' && portfolio.webLendingSessions?.UPBIT === false;

  return (
    <>
      {/* 1. Page Title Bar */}
      <div className="page-header">
        <div>
          <h1 className="page-title">
            {viewMode === 'PAPER' ? t.dashboard.titlePaper : t.dashboard.titleLive}
          </h1>
          <p className="page-subtitle">
            {viewMode === 'PAPER' ? t.dashboard.subPaper : t.dashboard.subLive}
          </p>
        </div>

        <span className={`badge ${sseConnected ? 'badge-emerald' : 'badge-amber'}`}>
          <span className={`status-dot ${sseConnected ? 'running' : 'draining'}`} />
          <span>{sseConnected ? (lang === 'ko' ? '실시간 연동 중' : 'Live Stream') : (lang === 'ko' ? '연결 중...' : 'Connecting...')}</span>
        </span>
      </div>

      {/* Optional Web Lending Alert (Only in Live Mode when session expired) */}
      {isUpbitLendingDisconnected && (
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: '20px',
            padding: '18px 24px',
            borderRadius: 'var(--radius-md)',
            background: 'var(--rose-soft)',
            border: '1px solid rgba(244, 63, 94, 0.35)'
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', gap: '14px' }}>
            <AlertTriangle size={20} style={{ color: '#fb7185', flexShrink: 0 }} />
            <div>
              <div style={{ fontWeight: 700, color: '#fecdd3' }}>
                {t.dashboard.webLendingAlertTitle}
              </div>
              <div style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                {t.dashboard.webLendingAlertDesc}
              </div>
            </div>
          </div>
          <button
            type="button"
            className="btn btn-sm btn-danger"
            onClick={() => navigate('/settings?tab=exchange&openNoVNC=1')}
          >
            <span>{t.dashboard.webLendingConnectBtn}</span>
            <ExternalLink size={14} />
          </button>
        </div>
      )}

      {/* 2. Flat 4-Column Hero Metric Strip */}
      <section className="metric-strip">
        <div className="metric-item">
          <div className="metric-label">
            <span>{t.dashboard.totalEquity}</span>
            <span className={`badge ${viewMode === 'PAPER' ? 'badge-amber' : 'badge-emerald'}`}>
              {viewMode === 'PAPER' ? t.nav.paperMode : t.nav.liveMode}
            </span>
          </div>
          <div className="metric-value mono">
            ${displayTotalUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
          </div>
          <div className="metric-sub mono">
            <span>{lang === 'ko' ? '열린 포지션 손익' : 'Open PnL'}:</span>
            <span
              style={{
                fontWeight: 700,
                color:
                  totalUnrealizedPnl > 0
                    ? 'var(--emerald)'
                    : totalUnrealizedPnl < 0
                      ? 'var(--rose)'
                      : 'var(--text-secondary)'
              }}
            >
              {totalUnrealizedPnl >= 0 ? '+' : ''}${totalUnrealizedPnl.toFixed(2)}
            </span>
          </div>
        </div>

        {displayExchanges.map((ex) => {
          const isKrw = ex.quoteCurrency === 'KRW';
          return (
            <div key={ex.name} className="metric-item">
              <div className="metric-label">
                <span>
                  {isKrw
                    ? lang === 'ko'
                      ? `${ex.name} 가용 원화 (KRW)`
                      : `${ex.name} Available (KRW)`
                    : lang === 'ko'
                      ? `${ex.name} 가용 달러 (${ex.quoteCurrency || 'USD'})`
                      : `${ex.name} Available (${ex.quoteCurrency || 'USD'})`}
                </span>
                <span className="mono" style={{ color: 'var(--text-muted)' }}>
                  {ex.name}
                </span>
              </div>
              <div className="metric-value mono">
                {isKrw
                  ? `₩${Math.round(ex.nativeBalance || 0).toLocaleString()}`
                  : `$${Number(ex.usdValue || ex.nativeBalance || 0).toLocaleString(undefined, {
                      minimumFractionDigits: 2,
                      maximumFractionDigits: 2
                    })}`}
              </div>
              <div className="metric-sub mono">
                {isKrw
                  ? `≈ $${Number(ex.usdValue || 0).toLocaleString(undefined, {
                      minimumFractionDigits: 0,
                      maximumFractionDigits: 0
                    })} USD (${Math.round(ex.percent || 0)}%)`
                  : lang === 'ko'
                    ? `비중 ${Math.round(ex.percent || 0)}% · 롱/숏 증거금 및 매수 자금`
                    : `Share ${Math.round(ex.percent || 0)}% · Quote & margin liquidity`}
              </div>
            </div>
          );
        })}

        <div className="metric-item">
          <div className="metric-label">
            <span>{t.dashboard.capitalBalance}</span>
            <span className={`badge ${isSkewLow ? 'badge-amber' : 'badge-emerald'}`}>
              {isSkewLow ? t.dashboard.rebalanceActive : t.dashboard.rebalanceNormal}
            </span>
          </div>
          <div className="metric-value mono" style={{ fontSize: '22px' }}>
            {domesticPct}% : {foreignPct}%
          </div>
          <div>
            <div className="balance-ratio-track">
              <div
                className="balance-ratio-fill"
                style={{
                  width: `${foreignPct}%`,
                  background: isSkewLow ? 'var(--amber)' : 'var(--emerald)'
                }}
              />
            </div>
          </div>
        </div>
      </section>

      {/* 3. Active Arbitrage Positions Table */}
      <section className="section-block">
        <div className="section-header">
          <h2 className="section-title">
            <span>{t.dashboard.activePositionsTitle}</span>
            <span className="badge badge-neutral mono">{activePositions.length}</span>
          </h2>
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() => navigate('/opportunities')}
          >
            <span>{t.dashboard.goToMarketBtn}</span>
            <ArrowRight size={14} />
          </button>
        </div>

        <div className="table-surface">
          {activePositions.length === 0 ? (
            <div className="empty-state">
              <div style={{ fontSize: '16px', fontWeight: 600, color: 'var(--text-primary)' }}>
                {t.dashboard.noPositionsTitle}
              </div>
              <div style={{ fontSize: '14px', maxWidth: '520px' }}>
                {t.dashboard.noPositionsDesc}
              </div>
              <button
                type="button"
                className="btn btn-primary"
                style={{ marginTop: '8px' }}
                onClick={() => navigate('/opportunities')}
              >
                <span>{t.dashboard.goToMarketBtn}</span>
                <ArrowRight size={15} />
              </button>
            </div>
          ) : (
            <table className="clean-table">
              <thead>
                <tr>
                  <th>{t.dashboard.colAsset}</th>
                  <th>{t.dashboard.colStrategy}</th>
                  <th>{t.dashboard.colInvested}</th>
                  <th>{t.dashboard.colEntrySpread}</th>
                  <th>{t.dashboard.colPnl}</th>
                  <th>{t.dashboard.colStatus}</th>
                  <th style={{ textAlign: 'right' }}>{t.dashboard.colAction}</th>
                </tr>
              </thead>
              <tbody>
                {activePositions.map((pos) => {
                  const coin = String(pos.baseSymbol || 'BTC').split('-')[0];
                  const isPosPnl = pos.currentPnlUsd >= 0;
                  return (
                    <tr key={pos.id}>
                      <td>
                        <div style={{ fontWeight: 700, fontSize: '16px' }} className="mono">
                          {coin}
                        </div>
                        <div style={{ fontSize: '13px', color: 'var(--text-secondary)', marginTop: '2px' }}>
                          {pos.baseExchange} → {pos.foreignExchange}
                        </div>
                      </td>
                      <td style={{ fontWeight: 500 }}>
                        {formatStrategyLabel(pos.strategy, lang)}
                      </td>
                      <td className="mono" style={{ fontWeight: 600 }}>
                        ${pos.investedUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
                      </td>
                      <td className="mono">
                        {pos.entrySpreadPct >= 0 ? '+' : ''}
                        {pos.entrySpreadPct.toFixed(2)}%
                      </td>
                      <td className="mono">
                        <div
                          style={{
                            fontWeight: 700,
                            color: isPosPnl ? 'var(--emerald)' : 'var(--rose)'
                          }}
                        >
                          {isPosPnl ? '+' : ''}${pos.currentPnlUsd.toFixed(2)}
                        </div>
                        <div style={{ fontSize: '12px', color: 'var(--text-secondary)' }}>
                          ({pos.currentPnlPct >= 0 ? '+' : ''}{pos.currentPnlPct.toFixed(2)}%)
                        </div>
                      </td>
                      <td>
                        <span
                          className={`badge ${
                            pos.status === 'TRANSFERRING'
                              ? 'badge-blue'
                              : pos.status === 'REPAYING'
                                ? 'badge-amber'
                                : 'badge-emerald'
                          }`}
                        >
                          {formatPositionStatus(pos.status, lang)}
                        </span>
                      </td>
                      <td style={{ textAlign: 'right' }}>
                        <button
                          type="button"
                          className="btn btn-sm btn-danger"
                          onClick={() => handleClosePosition(pos)}
                        >
                          {t.dashboard.closePositionBtn}
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </section>
    </>
  );
};
