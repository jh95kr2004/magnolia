import React, { useState, useEffect, useMemo } from 'react';
import { apiClient } from '../../services/api';
import { useCurrency } from '../../contexts/CurrencyContext';
import { useI18n } from '../../contexts/LanguageContext';

const TIMEFRAMES = ['1H', '24H', '1W', '1M', '1Y', 'ALL'];

export const EquityChart = ({
  viewMode = 'PAPER',
  currentTotalUsd = 0,
  totalUnrealizedPnlUsd = 0,
}) => {
  const { lang, t } = useI18n();
  const {
    currency,
    setCurrency,
    supportedCurrencies,
    formatMoney,
    updateFxRate,
  } = useCurrency();

  const [range, setRange] = useState('24H');
  const [historyPoints, setHistoryPoints] = useState([]);
  const [hoverIdx, setHoverIdx] = useState(null);

  useEffect(() => {
    let cancelled = false;
    const fetchHistory = () => {
      apiClient
        .get(`/portfolio/history?mode=${viewMode}&range=${range}`)
        .then((res) => {
          if (cancelled || !res.data) return;
          if (res.data.fx_rate_krw) {
            updateFxRate('KRW', res.data.fx_rate_krw);
          }
          if (Array.isArray(res.data.points)) {
            setHistoryPoints(res.data.points);
          }
        })
        .catch(() => {});
    };
    fetchHistory();
    const timer = setInterval(fetchHistory, 30000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [viewMode, range, updateFxRate]);

  // Append or update the tail point with the live SSE totalEquityUsd
  const chartSeries = useMemo(() => {
    const base = Array.isArray(historyPoints) ? [...historyPoints] : [];
    const liveVal = Number(currentTotalUsd || 0);
    const nowIso = new Date().toISOString();

    if (base.length === 0) {
      const seed = liveVal > 0 ? liveVal : 71000;
      return [
        { timestamp: new Date(Date.now() - 3600 * 1000).toISOString(), total_equity_usd: seed },
        { timestamp: nowIso, total_equity_usd: seed },
      ];
    }

    if (liveVal > 0) {
      const last = base[base.length - 1];
      const lastTime = last?.timestamp ? new Date(last.timestamp).getTime() : 0;
      if (Date.now() - lastTime < 30000) {
        base[base.length - 1] = {
          ...last,
          total_equity_usd: liveVal,
        };
      } else {
        base.push({
          timestamp: nowIso,
          total_equity_usd: liveVal,
        });
      }
    }

    if (base.length === 1) {
      return [
        {
          ...base[0],
          timestamp: new Date(new Date(base[0].timestamp).getTime() - 60000).toISOString(),
        },
        base[0],
      ];
    }
    return base;
  }, [historyPoints, currentTotalUsd]);

  const firstPoint = chartSeries[0];
  const lastPoint = chartSeries[chartSeries.length - 1];
  const activePoint =
    hoverIdx !== null && chartSeries[hoverIdx] ? chartSeries[hoverIdx] : lastPoint;

  const displayEquityUsd =
    hoverIdx !== null && activePoint
      ? Number(activePoint.total_equity_usd || 0)
      : Number(currentTotalUsd > 0 ? currentTotalUsd : lastPoint?.total_equity_usd || 0);

  const startEquityUsd = Number(firstPoint?.total_equity_usd || displayEquityUsd || 0);
  const deltaUsd = displayEquityUsd - startEquityUsd;
  const deltaPct = startEquityUsd > 0 ? (deltaUsd / startEquityUsd) * 100 : 0;
  const isPositive = deltaUsd >= 0;

  // Build SVG path coordinates (viewBox 720 x 200)
  const svgGeometry = useMemo(() => {
    const width = 720;
    const height = 195;
    const padX = 8;
    const padTop = 18;
    const padBottom = 18;
    const usableW = width - padX * 2;
    const usableH = height - padTop - padBottom;

    const values = chartSeries.map((p) => Number(p.total_equity_usd || 0));
    const minVal = Math.min(...values);
    const maxVal = Math.max(...values);
    const span = maxVal - minVal;

    const coords = chartSeries.map((pt, idx) => {
      const x =
        chartSeries.length > 1
          ? padX + (idx / (chartSeries.length - 1)) * usableW
          : padX + usableW / 2;
      const val = Number(pt.total_equity_usd || 0);
      const normY = span > 1e-4 ? (val - minVal) / span : 0.5;
      const y = padTop + usableH - normY * usableH;
      return { x, y, pt };
    });

    const linePath = coords
      .map((c, i) => `${i === 0 ? 'M' : 'L'} ${c.x.toFixed(1)} ${c.y.toFixed(1)}`)
      .join(' ');
    const firstX = coords[0]?.x ?? padX;
    const lastX = coords[coords.length - 1]?.x ?? width - padX;
    const areaPath = `${linePath} L ${lastX.toFixed(1)} ${height} L ${firstX.toFixed(1)} ${height} Z`;

    return { width, height, coords, linePath, areaPath };
  }, [chartSeries]);

  const strokeColor = isPositive ? '#3b82f6' : '#f43f5e';
  const hoveredCoord =
    hoverIdx !== null && svgGeometry.coords[hoverIdx] ? svgGeometry.coords[hoverIdx] : null;

  return (
    <div className="equity-chart-card" data-testid="equity-chart-card">
      {/* Header Row: Total Equity + Delta on Left, Currency Switcher on Right */}
      <div className="equity-card-header">
        <div>
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '10px',
              marginBottom: '6px',
            }}
          >
            <span style={{ fontSize: '13px', fontWeight: 600, color: 'var(--text-secondary)' }}>
              {t.dashboard.totalEquity}
            </span>
            <span className={`badge ${viewMode === 'PAPER' ? 'badge-amber' : 'badge-emerald'}`}>
              {viewMode === 'PAPER' ? t.nav.paperMode : t.nav.liveMode}
            </span>
          </div>

          <div className="equity-hero-value mono" data-testid="total-equity-value">
            {formatMoney(displayEquityUsd)}
          </div>

          <div className="equity-delta-row mono">
            <span
              style={{
                color: isPositive ? 'var(--emerald)' : 'var(--rose)',
                fontWeight: 700,
              }}
            >
              {isPositive ? '↗' : '↘'} {formatMoney(deltaUsd, { signed: true })} (
              {isPositive ? '+' : ''}
              {deltaPct.toFixed(2)}%)
            </span>
            <span style={{ color: 'var(--text-muted)', fontSize: '12px' }}>{range}</span>
            <span style={{ color: 'var(--border-strong)' }}>|</span>
            <span style={{ fontSize: '12px', color: 'var(--text-secondary)' }}>
              {lang === 'ko' ? '미실현 손익' : 'Open PnL'}:{' '}
              <strong
                style={{
                  color:
                    totalUnrealizedPnlUsd > 0
                      ? 'var(--emerald)'
                      : totalUnrealizedPnlUsd < 0
                        ? 'var(--rose)'
                        : 'var(--text-secondary)',
                }}
              >
                {formatMoney(totalUnrealizedPnlUsd, { signed: true })}
              </strong>
            </span>
          </div>
        </div>

        {/* Extensible Display Currency Switcher (USD / KRW + future currencies) */}
        <div
          className="currency-switcher"
          data-testid="currency-switcher"
          role="group"
          aria-label="Display Currency"
        >
          {supportedCurrencies.map((cur) => (
            <button
              key={cur.code}
              type="button"
              data-testid={`currency-btn-${cur.code}`}
              className={`currency-btn ${currency === cur.code ? 'active' : ''}`}
              onClick={() => setCurrency(cur.code)}
            >
              {cur.code} ({cur.symbol})
            </button>
          ))}
        </div>
      </div>

      {/* Interactive Coinbase-Style Glowing SVG Area Chart */}
      <div
        style={{ position: 'relative', width: '100%', height: '195px' }}
        onMouseLeave={() => setHoverIdx(null)}
      >
        <svg
          viewBox={`0 0 ${svgGeometry.width} ${svgGeometry.height}`}
          preserveAspectRatio="none"
          style={{ width: '100%', height: '100%', overflow: 'visible', display: 'block' }}
          data-testid="equity-area-chart"
          onMouseMove={(e) => {
            const rect = e.currentTarget.getBoundingClientRect();
            if (!rect.width || chartSeries.length === 0) return;
            const relX = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
            const idx = Math.round(relX * (chartSeries.length - 1));
            setHoverIdx(idx);
          }}
        >
          <defs>
            <linearGradient id="magnoliaEquityFill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={strokeColor} stopOpacity="0.28" />
              <stop offset="100%" stopColor={strokeColor} stopOpacity="0.0" />
            </linearGradient>
            <pattern
              id="coinbaseDotGrid"
              width="16"
              height="16"
              patternUnits="userSpaceOnUse"
            >
              <circle cx="2" cy="2" r="1" fill="rgba(148, 163, 184, 0.12)" />
            </pattern>
          </defs>

          <rect
            x="0"
            y="0"
            width={svgGeometry.width}
            height={svgGeometry.height}
            fill="url(#coinbaseDotGrid)"
          />

          <path d={svgGeometry.areaPath} fill="url(#magnoliaEquityFill)" />
          <path
            d={svgGeometry.linePath}
            fill="none"
            stroke={strokeColor}
            strokeWidth="2.4"
            strokeLinecap="round"
            strokeLinejoin="round"
          />

          {hoveredCoord && (
            <>
              <line
                x1={hoveredCoord.x}
                y1={0}
                x2={hoveredCoord.x}
                y2={svgGeometry.height}
                stroke="rgba(148, 163, 184, 0.35)"
                strokeDasharray="3 3"
                strokeWidth="1.2"
              />
              <circle
                cx={hoveredCoord.x}
                cy={hoveredCoord.y}
                r="5"
                fill={strokeColor}
                stroke="#0b0e14"
                strokeWidth="2"
              />
            </>
          )}
        </svg>

        {hoveredCoord && hoveredCoord.pt?.timestamp && (
          <div
            className="mono"
            style={{
              position: 'absolute',
              top: '6px',
              right: '10px',
              padding: '4px 10px',
              borderRadius: '6px',
              background: 'rgba(17, 22, 32, 0.92)',
              border: '1px solid var(--border-strong)',
              fontSize: '11px',
              color: 'var(--text-secondary)',
              pointerEvents: 'none',
            }}
          >
            {new Date(hoveredCoord.pt.timestamp).toLocaleString()} ·{' '}
            <strong style={{ color: 'var(--text-primary)' }}>
              {formatMoney(hoveredCoord.pt.total_equity_usd)}
            </strong>
          </div>
        )}
      </div>

      {/* Coinbase-Style Timeframe Selector Bar */}
      <div className="timeframe-tabs" role="tablist" aria-label="Chart Timeframe">
        {TIMEFRAMES.map((tf) => (
          <button
            key={tf}
            type="button"
            role="tab"
            aria-selected={range === tf}
            data-testid={`timeframe-tab-${tf}`}
            className={`timeframe-tab mono ${range === tf ? 'active' : ''}`}
            onClick={() => setRange(tf)}
          >
            {tf}
          </button>
        ))}
      </div>
    </div>
  );
};
