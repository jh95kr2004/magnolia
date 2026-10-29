import React, { useState, useEffect, useRef } from 'react';
import { RotateCcw, ChevronDown, ChevronUp } from 'lucide-react';
import { apiClient } from '../../services/api';
import { buildSseUrl, useAuth } from '../../contexts/AuthContext';
import { useI18n } from '../../contexts/LanguageContext';
import { formatStrategyLabel } from '../../i18n/translations';
import { useToast } from '../../contexts/ToastContext';
import { ColumnTooltipHeader } from '../../components/common/ColumnTooltipHeader';
import { PremiumTrajectoryChart } from '../../components/opportunities/PremiumTrajectoryChart';

const normalizeStrategyCode = (raw) => {
  const s = String(raw || '').toUpperCase();
  if (s.includes('TRANSFER') || s.includes('(C)')) return 'FOREIGN_TRANSFER';
  if (s.includes('MARGIN_FUTURE') || s.includes('MARGIN-FUTURE') || s.includes('(B)')) return 'MARGIN_FUTURE';
  if (s.includes('FUTURE_SPOT') || s.includes('FUTURE-SPOT') || s.includes('(D)')) return 'FUTURE_SPOT';
  if (s.includes('FUTURE_FUTURE') || s.includes('FUTURE-FUTURE') || s.includes('(E)')) return 'FUTURE_FUTURE';
  return 'MARGIN_SPOT';
};

const formatHorizonEta = (offsetMin, lang = 'ko') => {
  const mins = Math.max(1, Math.round(Number(offsetMin || 60)));
  if (mins < 60) {
    return lang === 'ko' ? `${mins}분 뒤` : `in ${mins}m`;
  }
  const hrs = (mins / 60).toFixed(1).replace(/\.0$/, '');
  return lang === 'ko' ? `${hrs}시간 뒤` : `in ${hrs}h`;
};

export const Opportunities = () => {
  const { viewMode } = useAuth();
  const { lang, t } = useI18n();
  const { showToast, requestConfirm } = useToast();

  const [opportunities, setOpportunities] = useState([]);
  const [expandedCoin, setExpandedCoin] = useState('BTC');
  const rawMapRef = useRef({});
  const aiMapRef = useRef({});
  const [simStatus, setSimStatus] = useState({
    exchange: 'UPBIT',
    current_offset_pct: 0,
    target_offset_pct: 0,
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
  const [applyingSim, setApplyingSim] = useState(false);
  const [enteringKey, setEnteringKey] = useState(null);

  useEffect(() => {
    apiClient
      .get('/settings/sim_offset')
      .then((res) => {
        if (res.data) setSimStatus(res.data);
      })
      .catch(() => {});

    const oppsSrc = new EventSource(buildSseUrl('/api/ws/opportunities', viewMode), {
      withCredentials: true
    });
    const dashSrc = new EventSource(buildSseUrl('/api/ws/dashboard', viewMode), {
      withCredentials: true
    });

    const rebuildMergedList = () => {
      const rawEntries = Object.values(rawMapRef.current);
      const aiByCoin = aiMapRef.current;

      if (rawEntries.length > 0) {
        const merged = rawEntries
          .filter((r) => Math.abs(Number(r.premium_pct || 0)) < 80)
          .map((item, idx) => {
            const coin = String(item.base_symbol || 'BTC-KRW').split('-')[0].toUpperCase();
            const aiInfo = aiByCoin[coin];
            const livePremPct = Number(item.premium_pct ?? 0);
            const predPct =
              aiInfo && typeof aiInfo.predicted_premium_pct === 'number'
                ? Number(aiInfo.predicted_premium_pct)
                : livePremPct * 0.65;
            const meanPct =
              aiInfo && typeof aiInfo.mean_premium_pct === 'number'
                ? Number(aiInfo.mean_premium_pct)
                : predPct;
            const targetOffsetMin =
              aiInfo && Number(aiInfo.target_offset_min) > 0
                ? Number(aiInfo.target_offset_min)
                : 60;
            const direction =
              aiInfo?.direction ||
              (predPct > livePremPct ? 'WIDENING' : 'NARROWING');
            const strategyCode = aiInfo?.strategy
              ? normalizeStrategyCode(aiInfo.strategy)
              : livePremPct >= 1.0
                ? 'FOREIGN_TRANSFER'
                : 'MARGIN_SPOT';

            const directionalEdge = Math.abs(livePremPct - predPct);
            const expectedMarginPct =
              aiInfo && typeof aiInfo.expected_roi === 'number' && Math.abs(aiInfo.expected_roi) < 0.8
                ? Math.max(Number(aiInfo.expected_roi) * 100, directionalEdge - 0.13)
                : Math.max(directionalEdge - 0.13, Number(item.expected_margin ?? Math.abs(livePremPct)) - 0.13);

            const targetWeightPct =
              aiInfo && typeof aiInfo.target_weight === 'number' && aiInfo.target_weight > 0
                ? Math.round(aiInfo.target_weight * 100)
                : Math.abs(livePremPct) >= 1.5
                  ? 35
                  : 20;

            return {
              id: `${item.base_exchange}-${item.foreign_exchange}-${item.base_symbol}-${idx}`,
              coin,
              baseExchange: item.base_exchange || 'UPBIT',
              foreignExchange: item.foreign_exchange || 'COINBASE',
              baseSymbol: item.base_symbol || `${coin}-KRW`,
              foreignSymbol: item.foreign_symbol || `${coin}-USD`,
              basePriceUsd: Number(item.base_price_usd || 0),
              foreignPriceUsd: Number(item.foreign_price_usd || 0),
              premiumPct: livePremPct,
              predictedPct: predPct,
              meanPct,
              targetOffsetMin,
              direction,
              expectedMarginPct,
              strategy: strategyCode,
              targetWeightPct
            };
          });
        setOpportunities(merged);
      } else {
        const aiEntries = Object.values(aiByCoin);
        if (aiEntries.length > 0) {
          const fallbackList = aiEntries
            .filter((a) => Math.abs(Number(a.current_premium_pct || 0)) < 80)
            .map((item, idx) => {
              const coin = String(item.base_symbol || 'BTC-KRW').split('-')[0].toUpperCase();
              const curPct = Number(item.current_premium_pct || 0);
              const predPct = Number(item.predicted_premium_pct ?? curPct * 0.65);
              const meanPct = Number(item.mean_premium_pct ?? predPct);
              const targetOffsetMin = Number(item.target_offset_min || 60);
              const direction = item.direction || (predPct > curPct ? 'WIDENING' : 'NARROWING');
              return {
                id: `${item.base_exchange}-${item.foreign_exchange}-${item.base_symbol}-${idx}`,
                coin,
                baseExchange: item.base_exchange || 'UPBIT',
                foreignExchange: item.foreign_exchange || 'COINBASE',
                baseSymbol: item.base_symbol || 'BTC-KRW',
                foreignSymbol: item.foreign_symbol || 'BTC-USD',
                basePriceUsd: Number(item.base_price_usd || 0),
                foreignPriceUsd: Number(item.foreign_price_usd || 0),
                premiumPct: curPct,
                predictedPct: predPct,
                meanPct,
                targetOffsetMin,
                direction,
                expectedMarginPct: Number(item.expected_roi || 0) * 100,
                strategy: normalizeStrategyCode(item.strategy),
                targetWeightPct:
                  typeof item.target_weight === 'number' && item.target_weight > 0
                    ? Math.round(item.target_weight * 100)
                    : 25
              };
            });
          setOpportunities(fallbackList);
        }
      }
    };

    oppsSrc.addEventListener('raw_opportunities', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (Array.isArray(data)) {
          const nextMap = {};
          data.forEach((item) => {
            if (!item || !item.base_symbol) return;
            if (!String(item.base_symbol).endsWith('-KRW')) return;
            const coin = String(item.base_symbol).split('-')[0].toUpperCase();
            nextMap[coin] = item;
          });
          rawMapRef.current = nextMap;
          rebuildMergedList();
        }
      } catch {}
    });

    oppsSrc.addEventListener('filtered_opportunities', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (Array.isArray(data)) {
          const nextAi = { ...aiMapRef.current };
          data.forEach((item) => {
            if (!item || !item.base_symbol) return;
            if (Math.abs(Number(item.current_premium_pct || 0)) >= 80) return;
            if (item.base_exchange === 'UPBIT' && !String(item.base_symbol).endsWith('-KRW')) return;
            const coin = String(item.base_symbol).split('-')[0].toUpperCase();
            const existing = nextAi[coin];
            if (!existing || Number(item.expected_roi || -999) > Number(existing.expected_roi || -999)) {
              nextAi[coin] = item;
            }
          });
          aiMapRef.current = nextAi;
          rebuildMergedList();
        }
      } catch {}
    });

    dashSrc.addEventListener('portfolio', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data && data.sim_status) {
          setSimStatus(data.sim_status);
        }
      } catch {}
    });

    return () => {
      oppsSrc.close();
      dashSrc.close();
    };
  }, [viewMode]);

  const applySimOffset = async (exchange, targetPct, rampMin) => {
    setApplyingSim(true);
    try {
      const res = await apiClient.post('/settings/sim_offset', {
        exchange,
        target_offset_pct: Number(targetPct),
        ramp_minutes: Number(rampMin)
      });
      if (res.data?.sim_status) {
        setSimStatus(res.data.sim_status);
      }
      setSimForm({
        exchange,
        targetOffsetPct: Number(targetPct),
        rampMinutes: Number(rampMin)
      });
      showToast(
        lang === 'ko'
          ? `${exchange} 시세 오프셋(${Number(targetPct) >= 0 ? '+' : ''}${targetPct}% / ${rampMin}분)이 적용되었습니다.`
          : `Price offset (${targetPct}% over ${rampMin}m) applied to ${exchange}.`,
        'success'
      );
    } catch (err) {
      showToast(err.response?.data?.error || err.message, 'error');
    } finally {
      setApplyingSim(false);
    }
  };

  const handleManualEnter = (opp, e) => {
    if (e) e.stopPropagation();
    const coin = String(opp.baseSymbol).split('-')[0];
    const isWidening = opp.direction === 'WIDENING';
    const reqBaseEx = isWidening ? opp.foreignExchange : opp.baseExchange;
    const reqForeignEx = isWidening ? opp.baseExchange : opp.foreignExchange;
    const reqBaseSym = isWidening ? opp.foreignSymbol : opp.baseSymbol;
    const reqForeignSym = isWidening ? opp.baseSymbol : opp.foreignSymbol;

    requestConfirm({
      title: lang === 'ko' ? `${coin} 수동 포지션 진입` : `Enter ${coin} Arbitrage Position`,
      message:
        lang === 'ko'
          ? `경로: ${reqBaseEx} (${reqBaseSym}) 숏 ↔ ${reqForeignEx} (${reqForeignSym}) 롱\n현재 프리미엄: ${opp.premiumPct >= 0 ? '+' : ''}${opp.premiumPct.toFixed(2)}% → 목표: ${opp.predictedPct >= 0 ? '+' : ''}${opp.predictedPct.toFixed(2)}%\n전략: ${formatStrategyLabel(opp.strategy, lang, opp.direction)}\n\n즉시 델타 중립 양방향 주문을 실행하시겠습니까?`
          : `Route: ${reqBaseEx} (Short) ↔ ${reqForeignEx} (Long)\nSpread: ${opp.premiumPct.toFixed(2)}% → Target: ${opp.predictedPct.toFixed(2)}%\nExecute delta-neutral entry now?`,
      confirmText: t.market.enterBtn,
      cancelText: t.stopModal.cancel,
      onConfirm: async () => {
        setEnteringKey(opp.id);
        try {
          await apiClient.post('/positions/force_enter', {
            mode: viewMode,
            base_exchange: reqBaseEx,
            foreign_exchange: reqForeignEx,
            base_symbol: reqBaseSym,
            foreign_symbol: reqForeignSym,
            strategy: opp.strategy,
            investment_usd: 2500
          });
          showToast(
            lang === 'ko'
              ? `${coin} 포지션 진입 요청이 엔진으로 전송되었습니다.`
              : `Entry command sent for ${coin}.`,
            'success'
          );
        } catch (err) {
          showToast(err.response?.data?.error || err.message, 'error');
        } finally {
          setEnteringKey(null);
        }
      }
    });
  };

  const currentSimPct = Number(simStatus.current_offset_pct || 0);
  const targetSimPct = Number(simStatus.target_offset_pct || 0);

  return (
    <>
      <div className="page-header">
        <div>
          <h1 className="page-title">{t.market.title}</h1>
          <p className="page-subtitle">{t.market.subtitle}</p>
        </div>
      </div>

      {/* 1. Paper Mode Gradual Price Offset Simulator Strip */}
      {viewMode === 'PAPER' && (
        <section className="sim-bar">
          <div className="sim-bar-top">
            <div>
              <div style={{ fontSize: '16px', fontWeight: 700, marginBottom: '4px' }}>
                {t.market.simTitle}
              </div>
              <div style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                {t.market.simDesc}
              </div>
            </div>

            <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
              <span style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                {t.market.simCurrentStatus}:
              </span>
              <span
                className={`badge mono ${
                  Math.abs(currentSimPct) > 0.01 ? 'badge-amber' : 'badge-neutral'
                }`}
                style={{ fontSize: '14px', padding: '6px 12px' }}
              >
                {simStatus.exchange || 'UPBIT'} {currentSimPct >= 0 ? '+' : ''}
                {currentSimPct.toFixed(2)}%
                {Math.abs(targetSimPct - currentSimPct) > 0.05 &&
                  ` → ${targetSimPct >= 0 ? '+' : ''}${targetSimPct.toFixed(2)}%`}
              </span>
            </div>
          </div>

          {/* Preset Quick Buttons */}
          <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '10px' }}>
            <button
              type="button"
              disabled={applyingSim}
              className="btn btn-sm btn-ghost"
              onClick={() => applySimOffset('UPBIT', 3.5, 3)}
            >
              {t.market.simPresetKimchi}
            </button>
            <button
              type="button"
              disabled={applyingSim}
              className="btn btn-sm btn-ghost"
              onClick={() => applySimOffset('UPBIT', -1.5, 2)}
            >
              {t.market.simPresetDiscount}
            </button>
            <button
              type="button"
              disabled={applyingSim}
              className="btn btn-sm btn-ghost"
              onClick={() => applySimOffset('UPBIT', 4.0, 0)}
            >
              {t.market.simPresetInstant}
            </button>
            <button
              type="button"
              disabled={applyingSim}
              className="btn btn-sm btn-ghost"
              onClick={() => applySimOffset('UPBIT', 0, 0)}
            >
              <RotateCcw size={13} />
              <span>{t.market.simPresetReset}</span>
            </button>
          </div>

          <div className="divider" />

          {/* Custom Offset Inputs */}
          <form
            className="sim-controls-row"
            onSubmit={(e) => {
              e.preventDefault();
              applySimOffset(simForm.exchange, simForm.targetOffsetPct, simForm.rampMinutes);
            }}
          >
            <div className="field-group" style={{ minWidth: '160px' }}>
              <label className="field-label">{t.market.simExchange}</label>
              <select
                value={simForm.exchange}
                onChange={(e) => setSimForm({ ...simForm, exchange: e.target.value })}
                className="select"
              >
                <option value="UPBIT">UPBIT (업비트)</option>
                <option value="COINBASE">COINBASE (코인베이스)</option>
              </select>
            </div>

            <div className="field-group" style={{ width: '160px' }}>
              <label className="field-label">{t.market.simTargetPct}</label>
              <input
                type="number"
                step="0.1"
                min="-30"
                max="30"
                value={simForm.targetOffsetPct}
                onChange={(e) => setSimForm({ ...simForm, targetOffsetPct: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group" style={{ width: '160px' }}>
              <label className="field-label">{t.market.simRampMin}</label>
              <input
                type="number"
                step="0.5"
                min="0"
                max="120"
                value={simForm.rampMinutes}
                onChange={(e) => setSimForm({ ...simForm, rampMinutes: e.target.value })}
                className="input mono"
              />
            </div>

            <button
              type="submit"
              disabled={applyingSim}
              className="btn btn-primary"
              style={{ height: '42px' }}
            >
              {t.market.simApplyBtn}
            </button>
          </form>
        </section>
      )}

      {/* 2. Live Cross-Exchange Opportunities Table with Expandable Trajectory Chart */}
      <section className="section-block">
        <div className="section-header">
          <h2 className="section-title">
            <span>{t.market.tableTitle}</span>
            <span className="badge badge-neutral mono">{opportunities.length}</span>
          </h2>
          <span style={{ fontSize: '12px', color: 'var(--text-secondary)' }}>
            {lang === 'ko'
              ? '행을 클릭하면 과거 프리미엄 추이와 향후 8시간 AI 예측 궤적 그래프를 볼 수 있습니다.'
              : 'Click any row to inspect historical spread and the 8-hour AI forecast trajectory chart.'}
          </span>
        </div>

        <div className="table-surface">
          {opportunities.length === 0 ? (
            <div className="empty-state">
              <div style={{ fontSize: '15px' }}>{t.market.noOpps}</div>
            </div>
          ) : (
            <table className="clean-table">
              <thead>
                <tr>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colPair}
                      tooltip={t.tooltips?.colPair}
                    />
                  </th>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colBasePrice}
                      tooltip={t.tooltips?.colBasePrice}
                    />
                  </th>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colForeignPrice}
                      tooltip={t.tooltips?.colForeignPrice}
                    />
                  </th>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colSpread}
                      tooltip={t.tooltips?.colSpread}
                    />
                  </th>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colAiForecast}
                      tooltip={t.tooltips?.colAiForecast}
                    />
                  </th>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colRecommended}
                      tooltip={t.tooltips?.colRecommended}
                    />
                  </th>
                  <th>
                    <ColumnTooltipHeader
                      label={t.market.colNetMargin}
                      tooltip={t.tooltips?.colNetMargin}
                    />
                  </th>
                  <th style={{ textAlign: 'right' }}>
                    <ColumnTooltipHeader
                      label={t.market.colManual}
                      tooltip={t.tooltips?.colManual}
                      align="right"
                    />
                  </th>
                </tr>
              </thead>
              <tbody>
                {opportunities.map((opp) => {
                  const coin = String(opp.baseSymbol).split('-')[0];
                  const isPosSpread = opp.premiumPct >= 0;
                  const isExpanded = expandedCoin === coin;
                  const isWidening = opp.direction === 'WIDENING';
                  const etaText = formatHorizonEta(opp.targetOffsetMin, lang);
                  const dirBadgeText =
                    lang === 'ko'
                      ? isWidening
                        ? `${etaText} 최고`
                        : `${etaText} 최저`
                      : isWidening
                        ? `Peak ${etaText}`
                        : `Low ${etaText}`;

                  return (
                    <React.Fragment key={opp.id}>
                      <tr
                        onClick={() => setExpandedCoin(isExpanded ? null : coin)}
                        style={{
                          cursor: 'pointer',
                          background: isExpanded ? 'rgba(56, 189, 248, 0.05)' : undefined
                        }}
                      >
                        <td>
                          <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                            <span className="mono" style={{ fontWeight: 700, fontSize: '16px' }}>
                              {coin}
                            </span>
                            {isExpanded ? (
                              <ChevronUp size={15} style={{ color: 'var(--text-secondary)' }} />
                            ) : (
                              <ChevronDown size={15} style={{ color: 'var(--text-secondary)' }} />
                            )}
                          </div>
                          <div
                            style={{
                              fontSize: '13px',
                              color: 'var(--text-secondary)',
                              marginTop: '2px'
                            }}
                          >
                            {opp.baseExchange} ({opp.baseSymbol}) → {opp.foreignExchange} (
                            {opp.foreignSymbol})
                          </div>
                        </td>
                        <td className="mono">
                          $
                          {opp.basePriceUsd.toLocaleString(undefined, {
                            minimumFractionDigits: 2,
                            maximumFractionDigits: 2
                          })}
                        </td>
                        <td className="mono">
                          $
                          {opp.foreignPriceUsd.toLocaleString(undefined, {
                            minimumFractionDigits: 2,
                            maximumFractionDigits: 2
                          })}
                        </td>
                        <td className="mono">
                          <span
                            className={`badge ${
                              opp.premiumPct >= 1.0
                                ? 'badge-emerald'
                                : opp.premiumPct <= -0.5
                                  ? 'badge-blue'
                                  : 'badge-neutral'
                            }`}
                            style={{ fontSize: '13px' }}
                          >
                            {isPosSpread ? '+' : ''}
                            {opp.premiumPct.toFixed(2)}%
                          </span>
                        </td>
                        <td>
                          <div
                            className="mono"
                            style={{
                              fontWeight: 700,
                              color: isWidening ? '#10b981' : '#f59e0b',
                              fontSize: '13.5px'
                            }}
                          >
                            {dirBadgeText}{' '}
                            {opp.predictedPct >= 0 ? '+' : ''}
                            {opp.predictedPct.toFixed(2)}%
                          </div>
                          <div
                            className="mono"
                            style={{
                              fontSize: '11.5px',
                              color: 'var(--text-secondary)',
                              marginTop: '2px'
                            }}
                          >
                            {lang === 'ko' ? '8H 평균:' : '8H Mean:'}{' '}
                            {(opp.meanPct ?? opp.predictedPct) >= 0 ? '+' : ''}
                            {Number(opp.meanPct ?? opp.predictedPct).toFixed(2)}%
                          </div>
                        </td>
                        <td style={{ fontWeight: 500 }}>
                          {formatStrategyLabel(opp.strategy, lang, opp.direction)}
                        </td>
                        <td
                          className="mono"
                          style={{
                            fontWeight: 700,
                            color:
                              opp.expectedMarginPct > 0.3
                                ? 'var(--emerald)'
                                : 'var(--text-secondary)'
                          }}
                        >
                          {opp.expectedMarginPct >= 0 ? '+' : ''}
                          {opp.expectedMarginPct.toFixed(2)}%
                        </td>
                        <td style={{ textAlign: 'right' }}>
                          <button
                            type="button"
                            disabled={enteringKey === opp.id}
                            onClick={(e) => handleManualEnter(opp, e)}
                            className="btn btn-sm btn-ghost"
                          >
                            {t.market.enterBtn}
                          </button>
                        </td>
                      </tr>

                      {isExpanded && (
                        <tr>
                          <td colSpan={8} style={{ padding: 0 }}>
                            <PremiumTrajectoryChart
                              baseExchange={opp.baseExchange}
                              baseSymbol={opp.baseSymbol}
                              foreignExchange={opp.foreignExchange}
                              foreignSymbol={opp.foreignSymbol}
                              viewMode={viewMode}
                              livePremiumPct={opp.premiumPct}
                              lang={lang}
                            />
                          </td>
                        </tr>
                      )}
                    </React.Fragment>
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
