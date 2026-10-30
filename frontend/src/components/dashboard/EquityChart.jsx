import React, { useState, useEffect, useMemo, useRef } from 'react';
import { apiClient } from '../../services/api';
import { useCurrency } from '../../contexts/CurrencyContext';
import { useI18n } from '../../contexts/LanguageContext';

const TIMEFRAMES = ['1H', '24H', '1W', '1M', '1Y', 'ALL'];

export const EquityChart = ({
  viewMode = 'PAPER',
  currentTotalUsd = 0,
  totalUnrealizedPnlUsd = 0,
  refreshKey = 0,
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
  const chartContainerRef = useRef(null);
  const [containerWidth, setContainerWidth] = useState(720);

  useEffect(() => {
    const el = chartContainerRef.current;
    if (!el) return;
    const measure = () => {
      if (el.clientWidth && el.clientWidth > 180) {
        setContainerWidth(el.clientWidth);
      }
    };
    measure();
    let ro = null;
    if (typeof ResizeObserver !== 'undefined') {
      ro = new ResizeObserver(measure);
      ro.observe(el);
    }
    window.addEventListener('resize', measure);
    return () => {
      if (ro) ro.disconnect();
      window.removeEventListener('resize', measure);
    };
  }, []);

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
  }, [viewMode, range, updateFxRate, refreshKey]);

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

  // Build SVG path coordinates with Y-axis anchored at 0 at the bottom
  const svgGeometry = useMemo(() => {
    const width = Math.max(240, containerWidth);
    const height = 195;
    const padX = 8;
    const padTop = 18;
    const padBottom = 14;
    const usableW = width - padX * 2;
    const usableH = height - padTop - padBottom;
    const bottomY = padTop + usableH;

    const values = chartSeries.map((p) => Math.max(0, Number(p.total_equity_usd || 0)));
    const minVal = 0;
    const peakVal = Math.max(0, ...values);
    const maxVal = peakVal > 0 ? peakVal * 1.16 : 100;
    const span = maxVal - minVal;

    const toY = (val) => {
      const clamped = Math.max(minVal, Math.min(maxVal, Number(val || 0)));
      const normY = span > 1e-4 ? (clamped - minVal) / span : 0;
      return bottomY - normY * usableH;
    };

    const coords = chartSeries.map((pt, idx) => {
      const x =
        chartSeries.length > 1
          ? padX + (idx / (chartSeries.length - 1)) * usableW
          : padX + usableW / 2;
      const val = Number(pt.total_equity_usd || 0);
      const y = toY(val);
      return { x, y, pt };
    });

    const linePath = coords
      .map((c, i) => `${i === 0 ? 'M' : 'L'} ${c.x.toFixed(1)} ${c.y.toFixed(1)}`)
      .join(' ');
    const firstX = coords[0]?.x ?? padX;
    const lastX = coords[coords.length - 1]?.x ?? width - padX;
    const areaPath = `${linePath} L ${lastX.toFixed(1)} ${bottomY.toFixed(1)} L ${firstX.toFixed(1)} ${bottomY.toFixed(1)} Z`;

    const yTicks = [
      { value: peakVal, y: toY(peakVal) },
      { value: peakVal * 0.5, y: toY(peakVal * 0.5) },
      { value: 0, y: bottomY }
    ];

    return { width, height, padX, bottomY, coords, linePath, areaPath, yTicks };
  }, [chartSeries, containerWidth]);

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
        ref={chartContainerRef}
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

          {/* Horizontal Y-Axis Guide Lines (Peak, Mid, and 0 at bottom) */}
          {svgGeometry.yTicks.map((tick, idx) => (
            <line
              key={idx}
              x1={svgGeometry.padX}
              y1={tick.y}
              x2={svgGeometry.width - svgGeometry.padX}
              y2={tick.y}
              stroke={tick.value === 0 ? 'rgba(148, 163, 184, 0.25)' : 'rgba(148, 163, 184, 0.12)'}
              strokeDasharray={tick.value === 0 ? undefined : '3 3'}
              strokeWidth="1"
            />
          ))}

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
            <line
              x1={hoveredCoord.x}
              y1={0}
              x2={hoveredCoord.x}
              y2={svgGeometry.height}
              stroke="rgba(148, 163, 184, 0.35)"
              strokeDasharray="3 3"
              strokeWidth="1.2"
            />
          )}
        </svg>

        {/* Aspect-Ratio-Independent Circular Hover Point Indicator */}
        {hoveredCoord && (
          <div
            style={{
              position: 'absolute',
              left: `${(hoveredCoord.x / svgGeometry.width) * 100}%`,
              top: `${(hoveredCoord.y / svgGeometry.height) * 100}%`,
              width: '11px',
              height: '11px',
              borderRadius: '50%',
              background: strokeColor,
              border: '2px solid #0b0e14',
              boxShadow: `0 0 8px ${strokeColor}`,
              transform: 'translate(-50%, -50%)',
              pointerEvents: 'none',
            }}
          />
        )}

        {/* Crisp Y-Axis Value Labels (Peak, 50%, and 0 at bottom) */}
        {svgGeometry.yTicks.map((tick, idx) => (
          <div
            key={idx}
            className="mono"
            style={{
              position: 'absolute',
              left: '8px',
              top: `${(tick.y / svgGeometry.height) * 100}%`,
              transform: tick.value === 0 ? 'translateY(-100%)' : 'translateY(-50%)',
              padding: '1px 5px',
              borderRadius: '4px',
              background: 'rgba(11, 14, 20, 0.68)',
              fontSize: '10.5px',
              color: 'rgba(148, 163, 184, 0.85)',
              pointerEvents: 'none',
            }}
          >
            {formatMoney(tick.value)}
          </div>
        ))}

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
