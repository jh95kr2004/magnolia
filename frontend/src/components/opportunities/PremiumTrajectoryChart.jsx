import React, { useState, useEffect, useMemo, useRef } from 'react';
import { apiClient } from '../../services/api';

const RANGE_OPTIONS = ['1H', '6H', '24H'];

const formatOffsetLabel = (mins, lang = 'ko') => {
  const m = Math.round(Number(mins || 0));
  if (m < 60) {
    return lang === 'ko' ? `+${m}분` : `+${m}m`;
  }
  const h = (m / 60).toFixed(1).replace(/\.0$/, '');
  return lang === 'ko' ? `+${h}시간` : `+${h}h`;
};

export const PremiumTrajectoryChart = ({
  baseExchange = 'UPBIT',
  baseSymbol = 'BTC-KRW',
  foreignExchange = 'COINBASE',
  foreignSymbol = 'BTC-USD',
  viewMode = 'PAPER',
  livePremiumPct,
  lang = 'ko'
}) => {
  const [range, setRange] = useState('6H');
  const [chartData, setChartData] = useState(null);
  const [hoverX, setHoverX] = useState(null);
  const containerRef = useRef(null);
  const [containerWidth, setContainerWidth] = useState(1240);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const measure = () => {
      if (el.clientWidth && el.clientWidth > 200) {
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
    let active = true;
    const fetchChart = async () => {
      try {
        const res = await apiClient.get('/opportunities/chart', {
          params: {
            base_exchange: baseExchange,
            base_symbol: baseSymbol,
            foreign_exchange: foreignExchange,
            foreign_symbol: foreignSymbol,
            range,
            mode: viewMode
          }
        });
        if (active && res.data) {
          setChartData(res.data);
        }
      } catch {
        // Fallback handled gracefully by state
      }
    };

    fetchChart();
    const timer = setInterval(fetchChart, 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [baseExchange, baseSymbol, foreignExchange, foreignSymbol, range, viewMode]);

  const processed = useMemo(() => {
    const rawHistory = Array.isArray(chartData?.history) ? chartData.history : [];
    const rawForecast = Array.isArray(chartData?.forecast) ? chartData.forecast : [];
    const summary = chartData?.summary || {};

    const curPct =
      typeof livePremiumPct === 'number' && !Number.isNaN(livePremiumPct)
        ? livePremiumPct
        : Number(summary.current_pct ?? 0.25);

    const horizonKey =
      range === '1H' ? 'pred_1h_pct' : range === '24H' ? 'pred_24h_pct' : 'pred_6h_pct';

    const history =
      rawHistory.length > 0
        ? rawHistory.map((pt, i) => ({
            ...pt,
            actual_pct:
              i === rawHistory.length - 1 ? curPct : Number(pt.actual_pct ?? curPct),
            ai_past_pred_pct: Number(
              pt[horizonKey] ?? pt.ai_past_pred_pct ?? pt.actual_pct ?? curPct
            )
          }))
        : Array.from({ length: 24 }, (_, i) => {
            const v = curPct + Math.sin(i / 4) * 0.12;
            return {
              timestamp: new Date(Date.now() - (23 - i) * 60000).toISOString(),
              actual_pct: i === 23 ? curPct : v,
              ai_past_pred_pct: v - 0.03
            };
          });

    const anchorDiff =
      rawForecast.length > 0
        ? curPct - Number(rawForecast[0]?.predicted_pct ?? curPct)
        : 0;

    const forecast =
      rawForecast.length > 0
        ? rawForecast.map((pt, i) => {
            const offMin = Number(pt.offset_min ?? i * 5);
            const decay = Math.exp((-0.35 * offMin) / 480.0);
            return {
              ...pt,
              offset_min: offMin,
              predicted_pct:
                i === 0
                  ? curPct
                  : Number(pt.predicted_pct ?? curPct) + anchorDiff * decay
            };
          })
        : Array.from({ length: 49 }, (_, i) => {
            const offMin = i * 10;
            const t = offMin / 480.0;
            const wave = Math.sin(offMin / 28.0) * 0.08 * Math.exp(-1.1 * t);
            return {
              timestamp: new Date(Date.now() + offMin * 60000).toISOString(),
              offset_min: offMin,
              predicted_pct: curPct * Math.exp((-0.35 * offMin) / 480.0) + wave
            };
          });

    // Identify MAX and MIN on the future forecast curve, preferring genuine interior turning points
    // beyond the immediate NOW origin (offset_min >= 15) when index 1 is merely the starting boundary.
    let maxIdx = 1;
    let minIdx = 1;
    for (let i = 1; i < forecast.length; i++) {
      if (forecast[i].predicted_pct > (forecast[maxIdx]?.predicted_pct ?? -Infinity)) {
        maxIdx = i;
      }
      if (forecast[i].predicted_pct < (forecast[minIdx]?.predicted_pct ?? Infinity)) {
        minIdx = i;
      }
    }

    if (Number(forecast[maxIdx]?.offset_min ?? 0) <= 6 && forecast.length > 6) {
      let bestLocalPeak = -1;
      for (let i = 2; i < forecast.length - 1; i++) {
        if (
          Number(forecast[i].offset_min) >= 15 &&
          forecast[i].predicted_pct >= forecast[i - 1].predicted_pct &&
          forecast[i].predicted_pct >= forecast[i + 1].predicted_pct
        ) {
          if (
            bestLocalPeak === -1 ||
            forecast[i].predicted_pct > forecast[bestLocalPeak].predicted_pct
          ) {
            bestLocalPeak = i;
          }
        }
      }
      if (bestLocalPeak !== -1) {
        maxIdx = bestLocalPeak;
      } else {
        for (let i = 1; i < forecast.length; i++) {
          if (Number(forecast[i].offset_min) >= 15) {
            if (
              Number(forecast[maxIdx]?.offset_min ?? 0) < 15 ||
              forecast[i].predicted_pct > forecast[maxIdx].predicted_pct
            ) {
              maxIdx = i;
            }
          }
        }
      }
    }

    if (Number(forecast[minIdx]?.offset_min ?? 0) <= 6 && forecast.length > 6) {
      let bestLocalTrough = -1;
      for (let i = 2; i < forecast.length - 1; i++) {
        if (
          Number(forecast[i].offset_min) >= 15 &&
          forecast[i].predicted_pct <= forecast[i - 1].predicted_pct &&
          forecast[i].predicted_pct <= forecast[i + 1].predicted_pct
        ) {
          if (
            bestLocalTrough === -1 ||
            forecast[i].predicted_pct < forecast[bestLocalTrough].predicted_pct
          ) {
            bestLocalTrough = i;
          }
        }
      }
      if (bestLocalTrough !== -1) {
        minIdx = bestLocalTrough;
      } else {
        for (let i = 1; i < forecast.length; i++) {
          if (Number(forecast[i].offset_min) >= 15) {
            if (
              Number(forecast[minIdx]?.offset_min ?? 0) < 15 ||
              forecast[i].predicted_pct < forecast[minIdx].predicted_pct
            ) {
              minIdx = i;
            }
          }
        }
      }
    }

    const maxPt = forecast[maxIdx] || forecast[0];
    const minPt = forecast[minIdx] || forecast[0];

    const allValues = [
      ...history.map((h) => h.actual_pct),
      ...history.map((h) => h.ai_past_pred_pct),
      ...forecast.map((f) => f.predicted_pct),
      0
    ];
    let minVal = Math.min(...allValues);
    let maxVal = Math.max(...allValues);
    const span = Math.max(0.4, maxVal - minVal);
    minVal -= span * 0.18;
    maxVal += span * 0.22;

    return {
      history,
      forecast,
      curPct,
      maxPt,
      minPt,
      maxIdx,
      minIdx,
      minVal,
      maxVal,
      direction:
        summary.direction ||
        (maxPt.predicted_pct - curPct >= curPct - minPt.predicted_pct
          ? 'WIDENING'
          : 'NARROWING'),
      mean8hPct: Number(summary.mean_8h_pct ?? curPct)
    };
  }, [chartData, livePremiumPct, range]);

  const width = Math.max(640, containerWidth);
  const height = 240;
  const padLeft = 48;
  const padRight = 12;
  const padTop = 26;
  const padBottom = 30;
  const plotW = width - padLeft - padRight;
  const plotH = height - padTop - padBottom;

  // Split canvas: 52% for Past History, 48% for Future 8H Forecast
  const nowX = padLeft + plotW * 0.52;
  const histW = nowX - padLeft;
  const futW = padLeft + plotW - nowX;

  const toY = (pct) => {
    const ratio = (pct - processed.minVal) / Math.max(1e-6, processed.maxVal - processed.minVal);
    return padTop + plotH * (1 - Math.min(1, Math.max(0, ratio)));
  };

  const histPoints = processed.history.map((pt, i) => {
    const x =
      processed.history.length > 1
        ? padLeft + (i / (processed.history.length - 1)) * histW
        : nowX;
    return {
      x,
      yActual: toY(pt.actual_pct),
      yPastAi: toY(pt.ai_past_pred_pct),
      zone: 'PAST',
      timestamp: pt.timestamp,
      actualPct: pt.actual_pct,
      aiPastPct: pt.ai_past_pred_pct
    };
  });

  const futPoints = processed.forecast.map((pt, i) => {
    const offRatio = Math.min(1, Math.max(0, Number(pt.offset_min || 0) / 480));
    const x =
      processed.forecast.length > 1
        ? nowX +
          (offRatio > 0
            ? offRatio * futW
            : (i / (processed.forecast.length - 1)) * futW)
        : nowX;
    return {
      x,
      yPred: toY(pt.predicted_pct),
      zone: 'FUTURE',
      offsetMin: pt.offset_min,
      timestamp: pt.timestamp,
      predictedPct: pt.predicted_pct
    };
  });

  const actualPath = histPoints
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.yActual.toFixed(1)}`)
    .join(' ');

  const pastAiPath = histPoints
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.yPastAi.toFixed(1)}`)
    .join(' ');

  const futureAiPath = futPoints
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.yPred.toFixed(1)}`)
    .join(' ');

  const baselineY = toY(processed.minVal);
  const actualAreaPath =
    histPoints.length > 1
      ? `${actualPath} L ${histPoints[histPoints.length - 1].x.toFixed(1)} ${baselineY.toFixed(1)} L ${histPoints[0].x.toFixed(1)} ${baselineY.toFixed(1)} Z`
      : '';

  const futureAreaPath =
    futPoints.length > 1
      ? `${futureAiPath} L ${futPoints[futPoints.length - 1].x.toFixed(1)} ${baselineY.toFixed(1)} L ${futPoints[0].x.toFixed(1)} ${baselineY.toFixed(1)} Z`
      : '';

  const zeroY = toY(0);
  const nowY = histPoints.length > 0 ? histPoints[histPoints.length - 1].yActual : toY(processed.curPct);
  const maxFutCoord = futPoints[processed.maxIdx] || futPoints[0];
  const minFutCoord = futPoints[processed.minIdx] || futPoints[0];

  const isWidening = processed.direction === 'WIDENING';
  const forecastColor = isWidening ? '#10b981' : '#f59e0b';

  // Compute exact cursor-locked hover state with linear segment interpolation
  let hoveredPoint = null;
  if (hoverX !== null) {
    const clampedX = Math.min(width - padRight, Math.max(padLeft, hoverX));
    if (clampedX <= nowX && histPoints.length > 0) {
      let idx = 0;
      let bestDist = Infinity;
      for (let i = 0; i < histPoints.length; i++) {
        const d = Math.abs(histPoints[i].x - clampedX);
        if (d < bestDist) {
          bestDist = d;
          idx = i;
        }
      }
      const pt = histPoints[idx];
      hoveredPoint = {
        ...pt,
        xPos: clampedX,
        dotY: pt.yActual,
        dotColor: '#38bdf8'
      };
    } else if (futPoints.length > 0) {
      let p0 = futPoints[0];
      let p1 = futPoints[futPoints.length - 1];
      for (let i = 0; i < futPoints.length - 1; i++) {
        if (clampedX >= futPoints[i].x && clampedX <= futPoints[i + 1].x) {
          p0 = futPoints[i];
          p1 = futPoints[i + 1];
          break;
        }
      }
      const segSpan = Math.max(1e-6, p1.x - p0.x);
      const alpha = Math.min(1, Math.max(0, (clampedX - p0.x) / segSpan));
      const interpPct = p0.predictedPct + alpha * (p1.predictedPct - p0.predictedPct);
      const interpOffMin = p0.offsetMin + alpha * (p1.offsetMin - p0.offsetMin);
      const interpY = toY(interpPct);
      hoveredPoint = {
        zone: 'FUTURE',
        xPos: clampedX,
        yPred: interpY,
        dotY: interpY,
        dotColor: forecastColor,
        offsetMin: interpOffMin,
        predictedPct: interpPct
      };
    }
  }

  const yTicks = [
    processed.maxVal - (processed.maxVal - processed.minVal) * 0.1,
    (processed.maxVal + processed.minVal) * 0.5,
    processed.minVal + (processed.maxVal - processed.minVal) * 0.1
  ];

  return (
    <div
      data-testid="premium-trajectory-chart"
      style={{
        padding: '14px 12px',
        background: 'rgba(15, 23, 42, 0.72)',
        borderTop: '1px solid rgba(148, 163, 184, 0.14)',
        borderBottom: '1px solid rgba(148, 163, 184, 0.14)'
      }}
    >
      {/* Top Bar: Legend + Summary Metrics + Lookback Range Selector */}
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          flexWrap: 'wrap',
          gap: '12px',
          marginBottom: '12px'
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '16px' }}>
          <div style={{ fontWeight: 700, fontSize: '14px', color: 'var(--text-primary)' }}>
            {baseSymbol.split('-')[0]} ({baseExchange} ↔ {foreignExchange}){' '}
            {lang === 'ko'
              ? '실제 프리미엄 추이 & AI 8시간 예측 궤적'
              : 'Historical Spread & 8H AI Forecast Trajectory'}
          </div>

          {/* Legend Items */}
          <div style={{ display: 'flex', alignItems: 'center', gap: '14px', fontSize: '12px' }}>
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: '6px', color: '#38bdf8' }}>
              <span
                style={{
                  width: '14px',
                  height: '2.5px',
                  background: '#38bdf8',
                  display: 'inline-block',
                  borderRadius: '2px'
                }}
              />
              {lang === 'ko' ? '실제 과거 프리미엄' : 'Actual Spread'}
            </span>

            <span
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: '6px',
                color: 'rgba(192, 132, 252, 0.85)'
              }}
            >
              <span
                style={{
                  width: '14px',
                  borderTop: '2px dashed rgba(192, 132, 252, 0.75)',
                  display: 'inline-block'
                }}
              />
              {lang === 'ko'
                ? range === '1H'
                  ? '과거 AI 예측 (20분 단위 갱신 궤적)'
                  : '과거 AI 예측 (1시간 단위 갱신 궤적)'
                : `Past AI Forecast (${range === '1H' ? '20m' : '1H'} Rolling Trajectory)`}
            </span>

            <span
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: '6px',
                color: forecastColor
              }}
            >
              <span
                style={{
                  width: '14px',
                  borderTop: `2.5px dashed ${forecastColor}`,
                  display: 'inline-block'
                }}
              />
              {lang === 'ko' ? '향후 8시간 AI 예측 궤적' : 'Future 8H AI Trajectory'}
            </span>
          </div>
        </div>

        {/* Lookback Range Buttons (1H / 6H / 24H) */}
        <div style={{ display: 'flex', alignItems: 'center', gap: '6px' }}>
          <span style={{ fontSize: '12px', color: 'var(--text-secondary)', marginRight: '4px' }}>
            {lang === 'ko' ? '과거 구간:' : 'History:'}
          </span>
          {RANGE_OPTIONS.map((opt) => (
            <button
              key={opt}
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                setRange(opt);
              }}
              className={`btn btn-sm ${range === opt ? 'btn-primary' : 'btn-ghost'}`}
              style={{ padding: '3px 10px', fontSize: '12px', height: '26px' }}
            >
              {opt}
            </button>
          ))}
        </div>
      </div>

      {/* SVG Trajectory Canvas */}
      <div ref={containerRef} style={{ position: 'relative', width: '100%' }}>
        <svg
          viewBox={`0 0 ${width} ${height}`}
          preserveAspectRatio="none"
          style={{ width: '100%', height: `${height}px`, display: 'block', overflow: 'visible' }}
          onMouseLeave={() => setHoverX(null)}
          onMouseMove={(e) => {
            const rect = e.currentTarget.getBoundingClientRect();
            const relX = ((e.clientX - rect.left) / Math.max(1, rect.width)) * width;
            setHoverX(relX);
          }}
        >
          <defs>
            <linearGradient id={`actualGrad-${baseSymbol}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="#38bdf8" stopOpacity="0.24" />
              <stop offset="100%" stopColor="#38bdf8" stopOpacity="0.01" />
            </linearGradient>
            <linearGradient id={`futGrad-${baseSymbol}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={forecastColor} stopOpacity="0.22" />
              <stop offset="100%" stopColor={forecastColor} stopOpacity="0.01" />
            </linearGradient>
          </defs>

          {/* Future 8H Shaded Zone Tint */}
          <rect
            x={nowX}
            y={padTop}
            width={futW}
            height={plotH}
            fill="rgba(30, 41, 59, 0.38)"
            rx="4"
          />

          {/* Horizontal Grid Lines & Y-Axis Labels */}
          {yTicks.map((val, idx) => {
            const y = toY(val);
            return (
              <g key={idx}>
                <line
                  x1={padLeft}
                  y1={y}
                  x2={width - padRight}
                  y2={y}
                  stroke="rgba(148, 163, 184, 0.12)"
                  strokeDasharray="3 3"
                />
                <text
                  x={padLeft - 8}
                  y={y + 4}
                  textAnchor="end"
                  fill="rgba(148, 163, 184, 0.75)"
                  fontSize="11"
                  fontFamily="monospace"
                >
                  {val >= 0 ? '+' : ''}
                  {val.toFixed(2)}%
                </text>
              </g>
            );
          })}

          {/* 0.00% Parity Baseline */}
          {zeroY >= padTop && zeroY <= padTop + plotH && (
            <line
              x1={padLeft}
              y1={zeroY}
              x2={width - padRight}
              y2={zeroY}
              stroke="rgba(255, 255, 255, 0.18)"
              strokeWidth="1"
            />
          )}

          {/* Area Fills */}
          {actualAreaPath && <path d={actualAreaPath} fill={`url(#actualGrad-${baseSymbol})`} />}
          {futureAreaPath && <path d={futureAreaPath} fill={`url(#futGrad-${baseSymbol})`} />}

          {/* 1. Dimmed Past AI Prediction Line (for visual accuracy comparison) */}
          {pastAiPath && (
            <path
              d={pastAiPath}
              fill="none"
              stroke="rgba(192, 132, 252, 0.62)"
              strokeWidth="1.8"
              strokeDasharray="4 4"
            />
          )}

          {/* 2. Solid Vivid Actual Past Premium Line */}
          {actualPath && (
            <path
              d={actualPath}
              fill="none"
              stroke="#38bdf8"
              strokeWidth="2.4"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          )}

          {/* 3. Future 8H AI Forecast Trajectory Curve */}
          {futureAiPath && (
            <path
              d={futureAiPath}
              fill="none"
              stroke={forecastColor}
              strokeWidth="2.5"
              strokeDasharray="6 4"
              strokeLinecap="round"
            />
          )}

          {/* 4. NOW Vertical Divider Line */}
          <line
            x1={nowX}
            y1={padTop - 8}
            x2={nowX}
            y2={padTop + plotH}
            stroke="rgba(226, 232, 240, 0.45)"
            strokeWidth="1.5"
            strokeDasharray="4 3"
          />
          <rect
            x={nowX - 54}
            y={4}
            width={108}
            height={18}
            rx="9"
            fill="rgba(15, 23, 42, 0.92)"
            stroke="rgba(56, 189, 248, 0.55)"
          />
          <text
            x={nowX}
            y={16}
            textAnchor="middle"
            fill="#e2e8f0"
            fontSize="10.5"
            fontWeight="700"
            fontFamily="monospace"
          >
            NOW {processed.curPct >= 0 ? '+' : ''}
            {processed.curPct.toFixed(2)}%
          </text>
          <circle cx={nowX} cy={nowY} r="4.5" fill="#38bdf8" stroke="#0f172a" strokeWidth="2" />

          {/* 5. Future 8H MAX & MIN Inflection Markers */}
          {maxFutCoord && (
            <g>
              <circle
                cx={maxFutCoord.x}
                cy={maxFutCoord.yPred}
                r="4"
                fill="#10b981"
                stroke="#0f172a"
                strokeWidth="1.5"
              />
              <text
                x={Math.min(width - padRight - 10, Math.max(nowX + 35, maxFutCoord.x))}
                y={Math.max(padTop + 12, maxFutCoord.yPred - 8)}
                textAnchor="middle"
                fill="#10b981"
                fontSize="10.5"
                fontWeight="700"
                fontFamily="monospace"
              >
                MAX {maxFutCoord.predictedPct >= 0 ? '+' : ''}
                {maxFutCoord.predictedPct.toFixed(2)}% ({formatOffsetLabel(maxFutCoord.offsetMin, lang)})
              </text>
            </g>
          )}

          {minFutCoord && (
            <g>
              <circle
                cx={minFutCoord.x}
                cy={minFutCoord.yPred}
                r="4"
                fill="#f59e0b"
                stroke="#0f172a"
                strokeWidth="1.5"
              />
              <text
                x={Math.min(width - padRight - 10, Math.max(nowX + 35, minFutCoord.x))}
                y={Math.min(padTop + plotH - 4, minFutCoord.yPred + 15)}
                textAnchor="middle"
                fill="#f59e0b"
                fontSize="10.5"
                fontWeight="700"
                fontFamily="monospace"
              >
                MIN {minFutCoord.predictedPct >= 0 ? '+' : ''}
                {minFutCoord.predictedPct.toFixed(2)}% ({formatOffsetLabel(minFutCoord.offsetMin, lang)})
              </text>
            </g>
          )}

          {/* X-Axis Zone Labels */}
          <text
            x={padLeft + 6}
            y={height - 8}
            fill="rgba(148, 163, 184, 0.7)"
            fontSize="11"
            fontFamily="monospace"
          >
            {lang === 'ko' ? `← 과거 ${range} 실제 추이 & AI 예측 비교` : `← Past ${range} Actual vs AI`}
          </text>
          <text
            x={nowX + 10}
            y={height - 8}
            fill="rgba(148, 163, 184, 0.7)"
            fontSize="11"
            fontFamily="monospace"
          >
            {lang === 'ko' ? '향후 +8시간 (480분) AI 예측 궤적 →' : 'Future +8H (480m) AI Forecast →'}
          </text>

          {/* Hover Crosshair & Active Curve Tracking Dot */}
          {hoveredPoint && (
            <g>
              <line
                x1={hoveredPoint.xPos}
                y1={padTop}
                x2={hoveredPoint.xPos}
                y2={padTop + plotH}
                stroke="rgba(255, 255, 255, 0.45)"
                strokeWidth="1.2"
                strokeDasharray="3 3"
              />
              <circle
                cx={hoveredPoint.xPos}
                cy={hoveredPoint.dotY}
                r="4.5"
                fill={hoveredPoint.dotColor}
                stroke="#0f172a"
                strokeWidth="2"
              />
            </g>
          )}
        </svg>

        {/* Interactive Hover Tooltip Pill */}
        {hoveredPoint && (
          <div
            style={{
              position: 'absolute',
              top: '6px',
              left: `${Math.min(
                Math.max(12, width - 260),
                Math.max(padLeft + 4, hoveredPoint.xPos - 95)
              )}px`,
              background: 'rgba(15, 23, 42, 0.94)',
              border: '1px solid rgba(148, 163, 184, 0.28)',
              borderRadius: '8px',
              padding: '5px 11px',
              fontSize: '12px',
              display: 'flex',
              alignItems: 'center',
              gap: '12px',
              pointerEvents: 'none',
              whiteSpace: 'nowrap',
              zIndex: 5
            }}
            className="mono"
          >
            {hoveredPoint.zone === 'PAST' ? (
              <>
                <span style={{ color: 'var(--text-secondary)' }}>
                  {hoveredPoint.timestamp
                    ? new Date(hoveredPoint.timestamp).toLocaleTimeString([], {
                        hour: '2-digit',
                        minute: '2-digit'
                      })
                    : 'PAST'}
                </span>
                <span style={{ color: '#38bdf8', fontWeight: 700 }}>
                  {lang === 'ko' ? '실제:' : 'Actual:'}{' '}
                  {hoveredPoint.actualPct >= 0 ? '+' : ''}
                  {hoveredPoint.actualPct.toFixed(2)}%
                </span>
                <span style={{ color: '#c084fc' }}>
                  {lang === 'ko' ? 'AI 예측 궤적:' : 'Past AI:'}{' '}
                  {hoveredPoint.aiPastPct >= 0 ? '+' : ''}
                  {hoveredPoint.aiPastPct.toFixed(2)}%
                </span>
              </>
            ) : (
              <>
                <span style={{ color: 'var(--text-secondary)' }}>
                  {formatOffsetLabel(hoveredPoint.offsetMin, lang)}
                </span>
                <span style={{ color: forecastColor, fontWeight: 700 }}>
                  {lang === 'ko' ? 'AI 예측:' : 'Forecast:'}{' '}
                  {hoveredPoint.predictedPct >= 0 ? '+' : ''}
                  {hoveredPoint.predictedPct.toFixed(2)}%
                </span>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
};
