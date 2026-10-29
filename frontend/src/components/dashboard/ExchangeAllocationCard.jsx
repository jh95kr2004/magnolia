import React from 'react';
import { useCurrency } from '../../contexts/CurrencyContext';
import { useI18n } from '../../contexts/LanguageContext';

const PIE_COLORS = ['#3b82f6', '#10b981', '#8b5cf6', '#f59e0b', '#06b6d4', '#ec4899'];

export const ExchangeAllocationCard = ({
  exchanges = [],
  foreignShare = 0.5,
  rebalanceTriggerSkew = 0.2,
}) => {
  const { lang, t } = useI18n();
  const { currency, formatMoney, formatNative } = useCurrency();

  const totalUsd = exchanges.reduce((acc, ex) => acc + Math.max(0, Number(ex.usdValue || 0)), 0);
  const isSkewLow = (foreignShare || 0.5) <= (rebalanceTriggerSkew || 0.2);

  // Normalize percentages so pie slices always sum to 100%
  const normalizedExchanges = exchanges.map((ex, idx) => {
    const rawPct =
      totalUsd > 0
        ? (Math.max(0, Number(ex.usdValue || 0)) / totalUsd) * 100
        : exchanges.length > 0
          ? 100 / exchanges.length
          : 50;
    return {
      ...ex,
      sharePct: rawPct,
      color: PIE_COLORS[idx % PIE_COLORS.length],
    };
  });

  // Donut SVG parameters
  const size = 116;
  const strokeWidth = 16;
  const radius = (size - strokeWidth) / 2;
  const circumference = 2 * Math.PI * radius;

  let cumulativePct = 0;
  const slices = normalizedExchanges.map((ex) => {
    const startPct = cumulativePct;
    cumulativePct += ex.sharePct;
    const dashLength = (ex.sharePct / 100) * circumference;
    const dashOffset = -((startPct / 100) * circumference);
    return {
      ...ex,
      dashArray: `${dashLength} ${Math.max(0, circumference - dashLength)}`,
      dashOffset,
    };
  });

  return (
    <div className="allocation-card" data-testid="exchange-allocation-card">
      {/* 1. Circular Donut / Pie Chart Section */}
      <div>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: '10px',
            marginBottom: '16px',
          }}
        >
          <span style={{ fontSize: '14px', fontWeight: 700, color: 'var(--text-primary)' }}>
            {t.dashboard.capitalBalance}
          </span>
          <span className={`badge ${isSkewLow ? 'badge-amber' : 'badge-emerald'}`}>
            {isSkewLow ? t.dashboard.rebalanceActive : t.dashboard.rebalanceNormal}
          </span>
        </div>

        <div className="pie-summary-row">
          <div style={{ position: 'relative', width: `${size}px`, height: `${size}px`, flexShrink: 0 }}>
            <svg
              width={size}
              height={size}
              viewBox={`0 0 ${size} ${size}`}
              data-testid="exchange-pie-chart"
              style={{ transform: 'rotate(-90deg)' }}
            >
              <circle
                cx={size / 2}
                cy={size / 2}
                r={radius}
                fill="transparent"
                stroke="rgba(148, 163, 184, 0.14)"
                strokeWidth={strokeWidth}
              />
              {slices.map((slice) => (
                <circle
                  key={slice.name}
                  cx={size / 2}
                  cy={size / 2}
                  r={radius}
                  fill="transparent"
                  stroke={slice.color}
                  strokeWidth={strokeWidth}
                  strokeDasharray={slice.dashArray}
                  strokeDashoffset={slice.dashOffset}
                  strokeLinecap="butt"
                />
              ))}
            </svg>
            <div
              className="mono"
              style={{
                position: 'absolute',
                inset: 0,
                display: 'flex',
                flexDirection: 'column',
                alignItems: 'center',
                justifyContent: 'center',
                pointerEvents: 'none',
              }}
            >
              <span style={{ fontSize: '11px', color: 'var(--text-muted)' }}>
                {lang === 'ko' ? '비중' : 'Split'}
              </span>
              <span style={{ fontSize: '13px', fontWeight: 700, color: 'var(--text-primary)' }}>
                {normalizedExchanges.map((e) => Math.round(e.sharePct)).join(':')}
              </span>
            </div>
          </div>

          <div className="pie-legend-list">
            {normalizedExchanges.map((ex) => (
              <div key={ex.name} className="pie-legend-item">
                <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                  <span
                    style={{
                      width: '10px',
                      height: '10px',
                      borderRadius: '50%',
                      background: ex.color,
                      flexShrink: 0,
                    }}
                  />
                  <span style={{ fontWeight: 600, color: 'var(--text-primary)' }}>{ex.name}</span>
                </div>
                <span className="mono" style={{ fontWeight: 700, color: 'var(--text-secondary)' }}>
                  {ex.sharePct.toFixed(1)}%
                </span>
              </div>
            ))}
          </div>
        </div>
      </div>

      {/* 2. Simple Exchange Available Currency List */}
      <div className="exchange-currency-list" data-testid="exchange-currency-list">
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            marginBottom: '2px',
          }}
        >
          <span style={{ fontSize: '13px', fontWeight: 700, color: 'var(--text-secondary)' }}>
            {t.dashboard.exchangeAvailableTitle ||
              (lang === 'ko' ? '거래소별 가용 통화' : 'Available Currency by Exchange')}
          </span>
          <span style={{ fontSize: '11px', color: 'var(--text-muted)' }}>
            {lang === 'ko' ? `환산 통화: ${currency}` : `Converted: ${currency}`}
          </span>
        </div>

        {normalizedExchanges.map((ex) => {
          const quote = String(ex.quoteCurrency || 'USD').toUpperCase();
          return (
            <div key={ex.name} className="exchange-currency-row" data-testid={`exchange-row-${ex.name}`}>
              <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                <span
                  style={{
                    width: '8px',
                    height: '8px',
                    borderRadius: '50%',
                    background: ex.color,
                    flexShrink: 0,
                  }}
                />
                <div>
                  <div style={{ fontWeight: 700, fontSize: '14px', color: 'var(--text-primary)' }}>
                    {ex.name}
                  </div>
                  <div style={{ fontSize: '11px', color: 'var(--text-muted)' }}>
                    {lang === 'ko' ? `가용 ${quote}` : `Available ${quote}`}
                  </div>
                </div>
              </div>

              <div className="mono" style={{ textAlign: 'right' }}>
                <div style={{ fontWeight: 700, fontSize: '14px', color: 'var(--text-primary)' }}>
                  {formatNative(ex.nativeBalance, quote)}
                </div>
                <div style={{ fontSize: '11px', color: 'var(--text-secondary)' }}>
                  ≈ {formatMoney(ex.usdValue)} ({Math.round(ex.sharePct)}%)
                </div>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
};
