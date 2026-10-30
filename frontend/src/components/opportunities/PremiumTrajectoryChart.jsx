import React, { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import { apiClient } from '../../services/api';

const PRESET_OPTIONS = [
  { id: '1H', label: '1H', start: -60, end: 120 },
  { id: '6H', label: '6H', start: -360, end: 480 },
  { id: '24H', label: '24H', start: -1440, end: 480 }
];

const chartDataCache = new Map();

const formatOffsetLabel = (mins, lang = 'ko') => {
  const m = Math.round(Number(mins || 0));
  if (m === 0) return 'NOW';
  const sign = m > 0 ? '+' : '-';
  const absM = Math.abs(m);
  if (absM < 60) {
    return lang === 'ko' ? `${sign}${absM}분` : `${sign}${absM}m`;
  }
  const h = (absM / 60).toFixed(1).replace(/\.0$/, '');
  return lang === 'ko' ? `${sign}${h}시간` : `${sign}${h}h`;
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
  const cacheKey = `${baseExchange}:${baseSymbol}:${foreignExchange}:${foreignSymbol}:${viewMode}`;
  const [chartData, setChartData] = useState(() => chartDataCache.get(cacheKey) || null);
  const [hoverX, setHoverX] = useState(null);
  const [viewWindow, setViewWindow] = useState({ start: -360, end: 480 });
  const [activePreset, setActivePreset] = useState('6H');
  const [isDragging, setIsDragging] = useState(false);

  const containerRef = useRef(null);
  const svgRef = useRef(null);
  const [containerWidth, setContainerWidth] = useState(1240);
  const dragRef = useRef({
    active: false,
    startClientX: 0,
    startViewStart: -360,
    startViewEnd: 480
  });

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

  // Always fetch the full 24H history + 8H forecast so the user can freely zoom and pan anywhere
  useEffect(() => {
    let active = true;
    const cached = chartDataCache.get(cacheKey);
    if (cached) {
      setChartData(cached);
    }
    const fetchChart = async () => {
      try {
        const res = await apiClient.get('/opportunities/chart', {
          params: {
            base_exchange: baseExchange,
            base_symbol: baseSymbol,
            foreign_exchange: foreignExchange,
            foreign_symbol: foreignSymbol,
            range: '24H',
            mode: viewMode
          }
        });
        if (active && res.data) {
          chartDataCache.set(cacheKey, res.data);
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
  }, [baseExchange, baseSymbol, foreignExchange, foreignSymbol, viewMode, cacheKey]);

  const width = Math.max(640, containerWidth);
  const height = 246;
  const padLeft = 48;
  const padRight = 12;
  const padTop = 26;
  const padBottom = 32;
  const plotW = width - padLeft - padRight;
  const plotH = height - padTop - padBottom;

  const processed = useMemo(() => {
    const rawHistory = Array.isArray(chartData?.history) ? chartData.history : [];
    const rawForecast = Array.isArray(chartData?.forecast) ? chartData.forecast : [];
    const summary = chartData?.summary || {};

    const curPct =
      typeof livePremiumPct === 'number' && !Number.isNaN(livePremiumPct)
        ? livePremiumPct
        : Number(summary.current_pct ?? 0.25);

    const viewSpan = Math.max(20, viewWindow.end - viewWindow.start);
    const useShortRolling = viewSpan <= 180;

    const baseHistory =
      rawHistory.length > 0
        ? rawHistory.map((pt, i) => {
            const offsetMin = -(rawHistory.length - 1 - i);
            const pastPred =
              pt.pred_24h_pct ?? pt.pred_6h_pct ?? pt.pred_1h_pct ?? pt.ai_past_pred_pct;
            return {
              ...pt,
              offsetMin,
              actual_pct:
                i === rawHistory.length - 1 ? curPct : Number(pt.actual_pct ?? curPct),
              ai_past_pred_pct: Number(pastPred ?? pt.actual_pct ?? curPct)
            };
          })
        : Array.from({ length: 361 }, (_, i) => {
            const offsetMin = -(360 - i);
            return {
              timestamp: new Date(Date.now() + offsetMin * 60000).toISOString(),
              offsetMin,
              actual_pct: curPct,
              ai_past_pred_pct: curPct
            };
          });

    // Smoothly anchor the final 18 minutes of the past AI trajectory to live curPct at NOW (offsetMin = 0)
    const lastPastPred =
      baseHistory.length > 0
        ? baseHistory[baseHistory.length - 1].ai_past_pred_pct
        : curPct;
    const pastTailDiff = curPct - lastPastPred;
    const tailWindow = 18;
    const history = baseHistory.map((pt, idx) => {
      if (idx === baseHistory.length - 1) {
        return { ...pt, ai_past_pred_pct: curPct };
      }
      if (pt.offsetMin > -tailWindow && pt.offsetMin <= 0) {
        const u = (pt.offsetMin + tailWindow) / tailWindow;
        const ramp = 0.5 * (1 - Math.cos(Math.PI * u));
        return {
          ...pt,
          ai_past_pred_pct: pt.ai_past_pred_pct + pastTailDiff * ramp
        };
      }
      return pt;
    });

    const anchorDiff =
      rawForecast.length > 0
        ? curPct - Number(rawForecast[0]?.predicted_pct ?? curPct)
        : 0;

    const forecast =
      rawForecast.length > 0
        ? rawForecast.map((pt, i) => {
            const offMin = Number(pt.offset_min ?? i * 2);
            const decay = Math.exp((-0.35 * offMin) / 480.0);
            return {
              ...pt,
              offset_min: offMin,
              offsetMin: offMin,
              predicted_pct:
                i === 0
                  ? curPct
                  : Number(pt.predicted_pct ?? curPct) + anchorDiff * decay
            };
          })
        : Array.from({ length: 49 }, (_, i) => {
            const offMin = i * 10;
            return {
              timestamp: new Date(Date.now() + offMin * 60000).toISOString(),
              offset_min: offMin,
              offsetMin: offMin,
              predicted_pct: curPct * Math.exp((-0.25 * offMin) / 480.0)
            };
          });

    // Identify MAX and MIN on the future forecast curve, preferring genuine interior turning points
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

    const minBound = history.length > 0 ? history[0].offsetMin : -1440;
    const maxBound = forecast.length > 0 ? forecast[forecast.length - 1].offsetMin : 480;

    // Auto-scale Y-axis to the currently visible [viewWindow.start, viewWindow.end] range
    const visibleVals = [];
    for (let i = 0; i < history.length; i++) {
      const h = history[i];
      if (h.offsetMin >= viewWindow.start - 5 && h.offsetMin <= viewWindow.end + 5) {
        visibleVals.push(h.actual_pct, h.ai_past_pred_pct);
      }
    }
    for (let i = 0; i < forecast.length; i++) {
      const f = forecast[i];
      if (f.offsetMin >= viewWindow.start - 5 && f.offsetMin <= viewWindow.end + 5) {
        visibleVals.push(f.predicted_pct);
      }
    }
    if (visibleVals.length === 0) {
      visibleVals.push(curPct, 0);
    }

    let minVal = Math.min(...visibleVals);
    let maxVal = Math.max(...visibleVals);
    const span = Math.max(0.25, maxVal - minVal);
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
      minBound,
      maxBound,
      useShortRolling,
      direction:
        summary.direction ||
        (maxPt.predicted_pct - curPct >= curPct - minPt.predicted_pct
          ? 'WIDENING'
          : 'NARROWING'),
      mean8hPct: Number(summary.mean_8h_pct ?? curPct)
    };
  }, [chartData, livePremiumPct, viewWindow.start, viewWindow.end]);

  const clampViewport = useCallback(
    (start, end) => {
      const minB = Math.min(-60, processed.minBound);
      const maxB = Math.max(60, processed.maxBound);
      const totalMaxSpan = maxB - minB;
      const span = Math.min(totalMaxSpan, Math.max(20, end - start));
      let s = start;
      let e = start + span;
      if (s < minB) {
        s = minB;
        e = minB + span;
      }
      if (e > maxB) {
        e = maxB;
        s = Math.max(minB, maxB - span);
      }
      return { start: s, end: e };
    },
    [processed.minBound, processed.maxBound]
  );

  // Attach non-passive wheel listener for smooth cursor-centered zoom
  useEffect(() => {
    const svgEl = svgRef.current;
    if (!svgEl) return;

    const handleWheel = (e) => {
      e.preventDefault();
      e.stopPropagation();

      const rect = svgEl.getBoundingClientRect();
      const relX = ((e.clientX - rect.left) / Math.max(1, rect.width)) * width;
      const cursorRatio = Math.min(1, Math.max(0, (relX - padLeft) / Math.max(1, plotW)));

      setViewWindow((prev) => {
        const curSpan = Math.max(20, prev.end - prev.start);
        const cursorTime = prev.start + cursorRatio * curSpan;
        const zoomFactor = e.deltaY < 0 ? 0.8 : 1.25;
        const minB = Math.min(-60, processed.minBound);
        const maxB = Math.max(60, processed.maxBound);
        const nextSpan = Math.min(maxB - minB, Math.max(20, curSpan * zoomFactor));
        const nextStart = cursorTime - cursorRatio * nextSpan;
        const nextEnd = nextStart + nextSpan;
        return clampViewport(nextStart, nextEnd);
      });
      setActivePreset(null);
    };

    svgEl.addEventListener('wheel', handleWheel, { passive: false });
    return () => {
      svgEl.removeEventListener('wheel', handleWheel);
    };
  }, [width, plotW, padLeft, processed.minBound, processed.maxBound, clampViewport]);

  // Attach window-level mousemove & mouseup listeners for smooth drag-panning
  useEffect(() => {
    if (!isDragging) return;

    const handleWindowMove = (e) => {
      if (!dragRef.current.active) return;
      const dxPixels = e.clientX - dragRef.current.startClientX;
      const startSpan = dragRef.current.startViewEnd - dragRef.current.startViewStart;
      const deltaMinutes = -(dxPixels / Math.max(1, plotW)) * startSpan;
      const next = clampViewport(
        dragRef.current.startViewStart + deltaMinutes,
        dragRef.current.startViewEnd + deltaMinutes
      );
      setViewWindow(next);
      setActivePreset(null);
    };

    const handleWindowUp = () => {
      dragRef.current.active = false;
      setIsDragging(false);
    };

    window.addEventListener('mousemove', handleWindowMove);
    window.addEventListener('mouseup', handleWindowUp);
    return () => {
      window.removeEventListener('mousemove', handleWindowMove);
      window.removeEventListener('mouseup', handleWindowUp);
    };
  }, [isDragging, plotW, clampViewport]);

  const applyPreset = (preset) => {
    setViewWindow(clampViewport(preset.start, preset.end));
    setActivePreset(preset.id);
  };

  const zoomByButton = (factor) => {
    setViewWindow((prev) => {
      const center = 0.5 * (prev.start + prev.end);
      const nextSpan = (prev.end - prev.start) * factor;
      return clampViewport(center - 0.5 * nextSpan, center + 0.5 * nextSpan);
    });
    setActivePreset(null);
  };

  const viewSpan = Math.max(1e-6, viewWindow.end - viewWindow.start);
  const toX = (offsetMin) => padLeft + ((offsetMin - viewWindow.start) / viewSpan) * plotW;
  const toY = (pct) => {
    const ratio = (pct - processed.minVal) / Math.max(1e-6, processed.maxVal - processed.minVal);
    return padTop + plotH * (1 - Math.min(1, Math.max(0, ratio)));
  };

  const nowX = toX(0);
  const nowVisible = nowX >= padLeft && nowX <= width - padRight;
  const futZoneLeft = Math.max(padLeft, Math.min(width - padRight, nowX));
  const futZoneWidth = Math.max(0, width - padRight - futZoneLeft);

  const histPoints = processed.history.map((pt) => ({
    x: toX(pt.offsetMin),
    offsetMin: pt.offsetMin,
    yActual: toY(pt.actual_pct),
    yPastAi: toY(pt.ai_past_pred_pct),
    zone: 'PAST',
    timestamp: pt.timestamp,
    actualPct: pt.actual_pct,
    aiPastPct: pt.ai_past_pred_pct
  }));

  const futPoints = processed.forecast.map((pt) => ({
    x: toX(pt.offsetMin),
    offsetMin: pt.offsetMin,
    yPred: toY(pt.predicted_pct),
    zone: 'FUTURE',
    timestamp: pt.timestamp,
    predictedPct: pt.predicted_pct
  }));

  const actualPath = histPoints
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.yActual.toFixed(1)}`)
    .join(' ');

  const pastAiPath = histPoints
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.yPastAi.toFixed(1)}`)
    .join(' ');

  const futureAiPath = futPoints
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.yPred.toFixed(1)}`)
    .join(' ');

  const baselineY = padTop + plotH;
  const actualAreaPath =
    histPoints.length > 1
      ? `${actualPath} L ${histPoints[histPoints.length - 1].x.toFixed(1)} ${baselineY.toFixed(1)} L ${histPoints[0].x.toFixed(1)} ${baselineY.toFixed(1)} Z`
      : '';

  const futureAreaPath =
    futPoints.length > 1
      ? `${futureAiPath} L ${futPoints[futPoints.length - 1].x.toFixed(1)} ${baselineY.toFixed(1)} L ${futPoints[0].x.toFixed(1)} ${baselineY.toFixed(1)} Z`
      : '';

  const zeroY = toY(0);
  const nowY =
    histPoints.length > 0 ? histPoints[histPoints.length - 1].yActual : toY(processed.curPct);
  const maxFutCoord = futPoints[processed.maxIdx] || futPoints[0];
  const minFutCoord = futPoints[processed.minIdx] || futPoints[0];

  const isWidening = processed.direction === 'WIDENING';
  const forecastColor = isWidening ? '#10b981' : '#f59e0b';

  // Compute exact cursor-locked hover state with linear segment interpolation
  let hoveredPoint = null;
  if (hoverX !== null && !isDragging) {
    const clampedX = Math.min(width - padRight, Math.max(padLeft, hoverX));
    const hoverOffsetMin = viewWindow.start + ((clampedX - padLeft) / plotW) * viewSpan;

    if (hoverOffsetMin <= 0 && histPoints.length > 0) {
      let idx = 0;
      let bestDist = Infinity;
      for (let i = 0; i < histPoints.length; i++) {
        const d = Math.abs(histPoints[i].offsetMin - hoverOffsetMin);
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
        if (
          hoverOffsetMin >= futPoints[i].offsetMin &&
          hoverOffsetMin <= futPoints[i + 1].offsetMin
        ) {
          p0 = futPoints[i];
          p1 = futPoints[i + 1];
          break;
        }
      }
      const segSpan = Math.max(1e-6, p1.offsetMin - p0.offsetMin);
      const alpha = Math.min(1, Math.max(0, (hoverOffsetMin - p0.offsetMin) / segSpan));
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
    processed.maxVal - (processed.maxVal - processed.minVal) * 0.12,
    (processed.maxVal + processed.minVal) * 0.5,
    processed.minVal + (processed.maxVal - processed.minVal) * 0.12
  ];

  // Dynamic X-axis time grid ticks based on current zoom span
  const xTickStep =
    viewSpan <= 60
      ? 10
      : viewSpan <= 180
        ? 30
        : viewSpan <= 420
          ? 60
          : viewSpan <= 900
            ? 120
            : 240;
  const firstTick = Math.ceil(viewWindow.start / xTickStep) * xTickStep;
  const xTicks = [];
  for (let m = firstTick; m <= viewWindow.end; m += xTickStep) {
    xTicks.push(m);
  }

  const clipId = `plotClip-${baseExchange}-${baseSymbol}-${foreignExchange}`;

  return (
    <div
      data-testid="premium-trajectory-chart"
      style={{
        padding: '14px 12px',
        background: 'rgba(15, 23, 42, 0.72)',
        borderTop: '1px solid rgba(148, 163, 184, 0.14)',
        borderBottom: '1px solid rgba(148, 163, 184, 0.14)',
        userSelect: 'none'
      }}
    >
      {/* Top Bar: Legend + Zoom/Pan Controls */}
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          flexWrap: 'wrap',
          gap: '12px',
          marginBottom: '10px'
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
            <span
              style={{ display: 'inline-flex', alignItems: 'center', gap: '6px', color: '#38bdf8' }}
            >
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
                ? '과거 AI 예측 (연속 추세·수렴 궤적)'
                : 'Past AI Forecast (Continuous Equilibrium Trajectory)'}
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

        {/* Zoom / Pan Hint + Quick Preset Buttons */}
        <div style={{ display: 'flex', alignItems: 'center', gap: '6px', flexWrap: 'wrap' }}>
          <span
            style={{
              fontSize: '11.5px',
              color: 'rgba(148, 163, 184, 0.75)',
              marginRight: '6px'
            }}
          >
            {lang === 'ko'
              ? '휠: 확대/축소 · 드래그: 이동 · 더블클릭: 초기화'
              : 'Wheel: Zoom · Drag: Pan · DblClick: Reset'}
          </span>

          <button
            type="button"
            title={lang === 'ko' ? '확대 (Zoom In)' : 'Zoom In'}
            onClick={(e) => {
              e.stopPropagation();
              zoomByButton(0.75);
            }}
            className="btn btn-sm btn-ghost mono"
            style={{ padding: '2px 8px', fontSize: '13px', height: '26px' }}
          >
            +
          </button>
          <button
            type="button"
            title={lang === 'ko' ? '축소 (Zoom Out)' : 'Zoom Out'}
            onClick={(e) => {
              e.stopPropagation();
              zoomByButton(1.35);
            }}
            className="btn btn-sm btn-ghost mono"
            style={{ padding: '2px 8px', fontSize: '13px', height: '26px' }}
          >
            −
          </button>

          {PRESET_OPTIONS.map((opt) => (
            <button
              key={opt.id}
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                applyPreset(opt);
              }}
              className={`btn btn-sm ${activePreset === opt.id ? 'btn-primary' : 'btn-ghost'}`}
              style={{ padding: '3px 10px', fontSize: '12px', height: '26px' }}
            >
              {opt.label}
            </button>
          ))}
        </div>
      </div>

      {/* SVG Trajectory Canvas */}
      <div ref={containerRef} style={{ position: 'relative', width: '100%' }}>
        <svg
          ref={svgRef}
          viewBox={`0 0 ${width} ${height}`}
          preserveAspectRatio="none"
          style={{
            width: '100%',
            height: `${height}px`,
            display: 'block',
            overflow: 'hidden',
            cursor: isDragging ? 'grabbing' : 'grab'
          }}
          onMouseDown={(e) => {
            if (e.button !== 0) return;
            e.stopPropagation();
            dragRef.current = {
              active: true,
              startClientX: e.clientX,
              startViewStart: viewWindow.start,
              startViewEnd: viewWindow.end
            };
            setIsDragging(true);
          }}
          onDoubleClick={(e) => {
            e.stopPropagation();
            applyPreset(PRESET_OPTIONS[1]); // Reset to default 6H + 8H view
          }}
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
            <clipPath id={clipId}>
              <rect x={padLeft} y={padTop - 8} width={plotW} height={plotH + 14} />
            </clipPath>
          </defs>

          {/* Future 8H Shaded Zone Tint */}
          {futZoneWidth > 0 && (
            <rect
              x={futZoneLeft}
              y={padTop}
              width={futZoneWidth}
              height={plotH}
              fill="rgba(30, 41, 59, 0.38)"
              rx="4"
            />
          )}

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

          {/* Vertical X-Axis Time Grid & Labels */}
          {xTicks.map((m) => {
            const x = toX(m);
            if (x < padLeft + 18 || x > width - padRight - 18) return null;
            return (
              <g key={m}>
                <line
                  x1={x}
                  y1={padTop}
                  x2={x}
                  y2={padTop + plotH}
                  stroke="rgba(148, 163, 184, 0.07)"
                />
                <text
                  x={x}
                  y={height - 8}
                  textAnchor="middle"
                  fill={m === 0 ? '#38bdf8' : 'rgba(148, 163, 184, 0.7)'}
                  fontWeight={m === 0 ? '700' : '400'}
                  fontSize="10.5"
                  fontFamily="monospace"
                >
                  {formatOffsetLabel(m, lang)}
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

          {/* Clipped Plot Curves & Markers */}
          <g clipPath={`url(#${clipId})`}>
            {actualAreaPath && (
              <path d={actualAreaPath} fill={`url(#actualGrad-${baseSymbol})`} />
            )}
            {futureAreaPath && (
              <path d={futureAreaPath} fill={`url(#futGrad-${baseSymbol})`} />
            )}

            {/* 1. Dimmed Past AI Prediction Line */}
            {pastAiPath && (
              <path
                d={pastAiPath}
                fill="none"
                stroke="rgba(192, 132, 252, 0.68)"
                strokeWidth="1.9"
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
            {nowVisible && (
              <>
                <line
                  x1={nowX}
                  y1={padTop - 8}
                  x2={nowX}
                  y2={padTop + plotH}
                  stroke="rgba(226, 232, 240, 0.45)"
                  strokeWidth="1.5"
                  strokeDasharray="4 3"
                />
                <circle
                  cx={nowX}
                  cy={nowY}
                  r="4.5"
                  fill="#38bdf8"
                  stroke="#0f172a"
                  strokeWidth="2"
                />
              </>
            )}

            {/* 5. Future 8H MAX & MIN Inflection Markers */}
            {maxFutCoord &&
              maxFutCoord.x >= padLeft &&
              maxFutCoord.x <= width - padRight && (
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
                    x={Math.min(width - padRight - 65, Math.max(padLeft + 65, maxFutCoord.x))}
                    y={Math.max(padTop + 14, maxFutCoord.yPred - 8)}
                    textAnchor="middle"
                    fill="#10b981"
                    fontSize="10.5"
                    fontWeight="700"
                    fontFamily="monospace"
                  >
                    MAX {maxFutCoord.predictedPct >= 0 ? '+' : ''}
                    {maxFutCoord.predictedPct.toFixed(2)}% (
                    {formatOffsetLabel(maxFutCoord.offsetMin, lang)})
                  </text>
                </g>
              )}

            {minFutCoord &&
              minFutCoord.x >= padLeft &&
              minFutCoord.x <= width - padRight && (
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
                    x={Math.min(width - padRight - 65, Math.max(padLeft + 65, minFutCoord.x))}
                    y={Math.min(padTop + plotH - 4, minFutCoord.yPred + 15)}
                    textAnchor="middle"
                    fill="#f59e0b"
                    fontSize="10.5"
                    fontWeight="700"
                    fontFamily="monospace"
                  >
                    MIN {minFutCoord.predictedPct >= 0 ? '+' : ''}
                    {minFutCoord.predictedPct.toFixed(2)}% (
                    {formatOffsetLabel(minFutCoord.offsetMin, lang)})
                  </text>
                </g>
              )}
          </g>

          {/* NOW Top Badge (rendered above clipPath when NOW is visible) */}
          {nowVisible && (
            <g>
              <rect
                x={Math.min(width - padRight - 108, Math.max(padLeft, nowX - 54))}
                y={4}
                width={108}
                height={18}
                rx="9"
                fill="rgba(15, 23, 42, 0.92)"
                stroke="rgba(56, 189, 248, 0.55)"
              />
              <text
                x={Math.min(width - padRight - 54, Math.max(padLeft + 54, nowX))}
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
            </g>
          )}

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
                Math.max(12, width - 270),
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
                    : formatOffsetLabel(hoveredPoint.offsetMin, lang)}
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
