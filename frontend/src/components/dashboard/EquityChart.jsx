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
  const [hoverRatio, setHoverRatio] = useState(null);
  const [zoomWindow, setZoomWindow] = useState(null);
  const [isDragging, setIsDragging] = useState(false);

  const chartContainerRef = useRef(null);
  const svgRef = useRef(null);
  const dragRef = useRef({
    active: false,
    moved: false,
    startX: 0,
    startMs: 0,
    endMs: 0,
  });
  const pinchRef = useRef({
    active: false,
    touchDriven: false,
    initialDist: 0,
    startSpan: 0,
    anchorTime: 0,
    midRatio: 0.5,
  });
  const singleTouchRef = useRef({
    active: false,
    startX: 0,
    startY: 0,
    lastX: 0,
    lastY: 0,
    mode: null,
  });
  const activeWindowRef = useRef({ startMs: 0, endMs: 0, minMs: 0, maxMs: 0, minSpanMs: 15000 });
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

  // Reset zoom window whenever range or viewMode changes
  useEffect(() => {
    setZoomWindow(null);
    setHoverRatio(null);
  }, [range, viewMode]);

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

  // Parse timestamps into sorted millisecond series
  const timedSeries = useMemo(() => {
    const pts = chartSeries
      .map((pt, i) => {
        const parsed = pt?.timestamp ? new Date(pt.timestamp).getTime() : NaN;
        const timeMs = Number.isFinite(parsed)
          ? parsed
          : Date.now() - (chartSeries.length - 1 - i) * 60000;
        return {
          ...pt,
          timeMs,
          val: Math.max(0, Number(pt?.total_equity_usd || 0)),
        };
      })
      .sort((a, b) => a.timeMs - b.timeMs);

    if (pts.length >= 2 && pts[pts.length - 1].timeMs <= pts[0].timeMs) {
      pts[0] = { ...pts[0], timeMs: pts[pts.length - 1].timeMs - 60000 };
    }
    return pts;
  }, [chartSeries]);

  const dataStartMs = timedSeries[0]?.timeMs ?? Date.now() - 3600 * 1000;
  const dataEndMs = Math.max(dataStartMs + 60000, timedSeries[timedSeries.length - 1]?.timeMs ?? Date.now());
  const fullSpanMs = Math.max(60000, dataEndMs - dataStartMs);
  const minZoomSpanMs = Math.max(15000, Math.min(60000, Math.floor(fullSpanMs * 0.02)));

  const winStartMs = zoomWindow
    ? Math.max(dataStartMs, Math.min(dataEndMs - minZoomSpanMs, zoomWindow.startMs))
    : dataStartMs;
  const winEndMs = zoomWindow
    ? Math.max(winStartMs + minZoomSpanMs, Math.min(dataEndMs, zoomWindow.endMs))
    : dataEndMs;

  useEffect(() => {
    activeWindowRef.current = {
      startMs: winStartMs,
      endMs: winEndMs,
      minMs: dataStartMs,
      maxMs: dataEndMs,
      minSpanMs: minZoomSpanMs,
    };
  }, [winStartMs, winEndMs, dataStartMs, dataEndMs, minZoomSpanMs]);

  const interpolateValueAt = (tMs) => {
    if (timedSeries.length === 0) return 0;
    if (tMs <= timedSeries[0].timeMs) return timedSeries[0].val;
    const last = timedSeries[timedSeries.length - 1];
    if (tMs >= last.timeMs) return last.val;

    for (let i = 0; i < timedSeries.length - 1; i++) {
      const p0 = timedSeries[i];
      const p1 = timedSeries[i + 1];
      if (tMs >= p0.timeMs && tMs <= p1.timeMs) {
        const dt = p1.timeMs - p0.timeMs;
        if (dt <= 0) return p1.val;
        const ratio = (tMs - p0.timeMs) / dt;
        return p0.val + ratio * (p1.val - p0.val);
      }
    }
    return last.val;
  };

  // Visible points within [winStartMs, winEndMs] with exact boundary interpolation
  const visibleSeries = useMemo(() => {
    const startVal = interpolateValueAt(winStartMs);
    const endVal = interpolateValueAt(winEndMs);
    const interior = timedSeries.filter(
      (pt) => pt.timeMs > winStartMs && pt.timeMs < winEndMs
    );
    return [
      {
        timestamp: new Date(winStartMs).toISOString(),
        timeMs: winStartMs,
        total_equity_usd: startVal,
        val: startVal,
      },
      ...interior,
      {
        timestamp: new Date(winEndMs).toISOString(),
        timeMs: winEndMs,
        total_equity_usd: endVal,
        val: endVal,
      },
    ];
  }, [timedSeries, winStartMs, winEndMs]);

  const applyZoomFactor = (factor, anchorRatio = 0.5) => {
    const { startMs, endMs, minMs, maxMs, minSpanMs } = activeWindowRef.current;
    const totalAvailable = Math.max(60000, maxMs - minMs);
    const curSpan = Math.max(minSpanMs, endMs - startMs);
    const nextSpan = Math.max(minSpanMs, Math.min(totalAvailable, curSpan * factor));

    if (nextSpan >= totalAvailable * 0.999) {
      setZoomWindow(null);
      return;
    }

    const clampedRatio = Math.max(0, Math.min(1, anchorRatio));
    const anchorTime = startMs + clampedRatio * curSpan;
    let nextStart = anchorTime - clampedRatio * nextSpan;
    let nextEnd = nextStart + nextSpan;

    if (nextStart < minMs) {
      nextStart = minMs;
      nextEnd = minMs + nextSpan;
    }
    if (nextEnd > maxMs) {
      nextEnd = maxMs;
      nextStart = Math.max(minMs, maxMs - nextSpan);
    }
    setZoomWindow({ startMs: nextStart, endMs: nextEnd });
  };

  // Non-passive wheel zoom + two-finger pinch-to-zoom + single-finger touch tooltip
  useEffect(() => {
    const targetEl = chartContainerRef.current;
    if (!targetEl) return;

    const getRect = () => (svgRef.current || targetEl).getBoundingClientRect();

    const handleWheel = (e) => {
      e.preventDefault();
      e.stopPropagation();
      const rect = getRect();
      if (!rect.width) return;
      const ratio = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
      const zoomFactor = e.deltaY < 0 ? 0.8 : 1.25;
      applyZoomFactor(zoomFactor, ratio);
    };

    const getTouchDist = (t1, t2) => {
      const dx = t1.clientX - t2.clientX;
      const dy = t1.clientY - t2.clientY;
      return Math.hypot(dx, dy);
    };

    const initPinch = (t0, t1) => {
      const rect = getRect();
      if (!rect.width) return;
      const dist = Math.max(10, getTouchDist(t0, t1));
      const midClientX = 0.5 * (t0.clientX + t1.clientX);
      const midRatio = Math.max(0, Math.min(1, (midClientX - rect.left) / rect.width));
      const { startMs, endMs } = activeWindowRef.current;
      const curSpan = Math.max(1000, endMs - startMs);
      pinchRef.current = {
        active: true,
        touchDriven: true,
        initialDist: dist,
        startSpan: curSpan,
        anchorTime: startMs + midRatio * curSpan,
        midRatio,
      };
      singleTouchRef.current.active = false;
      setHoverRatio(null);
    };

    const handleTouchStart = (e) => {
      if (!e.touches) return;
      if (e.touches.length >= 2) {
        if (e.cancelable) e.preventDefault();
        e.stopPropagation();
        initPinch(e.touches[0], e.touches[1]);
      } else if (e.touches.length === 1 && !pinchRef.current.active) {
        const t0 = e.touches[0];
        singleTouchRef.current = {
          active: true,
          startX: t0.clientX,
          startY: t0.clientY,
          lastX: t0.clientX,
          lastY: t0.clientY,
          mode: null,
        };
        const rect = getRect();
        if (!rect.width) return;
        const relX = Math.max(0, Math.min(1, (t0.clientX - rect.left) / rect.width));
        setHoverRatio(relX);
      }
    };

    const handleTouchMove = (e) => {
      if (!e.touches) return;
      if (e.touches.length >= 2) {
        if (e.cancelable) e.preventDefault();
        e.stopPropagation();
        const t0 = e.touches[0];
        const t1 = e.touches[1];
        if (!pinchRef.current.active) {
          initPinch(t0, t1);
        }
        const rect = getRect();
        if (!rect.width) return;
        const curDist = Math.max(10, getTouchDist(t0, t1));

        const { minMs, maxMs, minSpanMs } = activeWindowRef.current;
        const totalAvailable = Math.max(60000, maxMs - minMs);
        const scale = pinchRef.current.initialDist / curDist;
        const nextSpan = Math.max(
          minSpanMs,
          Math.min(totalAvailable, pinchRef.current.startSpan * scale)
        );

        if (nextSpan >= totalAvailable * 0.999) {
          setZoomWindow(null);
          return;
        }

        const midClientX = 0.5 * (t0.clientX + t1.clientX);
        const curMidRatio = Math.max(0, Math.min(1, (midClientX - rect.left) / rect.width));
        let nextStart = pinchRef.current.anchorTime - curMidRatio * nextSpan;
        let nextEnd = nextStart + nextSpan;

        if (nextStart < minMs) {
          nextStart = minMs;
          nextEnd = minMs + nextSpan;
        }
        if (nextEnd > maxMs) {
          nextEnd = maxMs;
          nextStart = Math.max(minMs, maxMs - nextSpan);
        }
        setZoomWindow({ startMs: nextStart, endMs: nextEnd });
      } else if (e.touches.length === 1 && !pinchRef.current.active) {
        const t0 = e.touches[0];
        const st = singleTouchRef.current;
        if (st.active) {
          const totalDx = Math.abs(t0.clientX - st.startX);
          const totalDy = Math.abs(t0.clientY - st.startY);
          const dy = t0.clientY - st.lastY;
          st.lastX = t0.clientX;
          st.lastY = t0.clientY;
          if (!st.mode && (totalDx > 5 || totalDy > 5)) {
            st.mode = totalDy > totalDx * 1.15 ? 'scroll-y' : 'scrub-x';
          }
          if (st.mode === 'scroll-y') {
            window.scrollBy(0, -dy);
            return;
          }
        }
        if (e.cancelable) e.preventDefault();
        const rect = getRect();
        if (!rect.width) return;
        const relX = Math.max(0, Math.min(1, (t0.clientX - rect.left) / rect.width));
        setHoverRatio(relX);
      }
    };

    const handleTouchEnd = (e) => {
      if (!e.touches || e.touches.length < 2) {
        pinchRef.current.active = false;
        pinchRef.current.touchDriven = false;
      }
      if (!e.touches || e.touches.length === 0) {
        singleTouchRef.current.active = false;
        singleTouchRef.current.mode = null;
      }
    };

    const handleGestureStart = (e) => {
      if (e.cancelable) e.preventDefault();
      if (!pinchRef.current.touchDriven) {
        const rect = getRect();
        const clientX = typeof e.clientX === 'number' ? e.clientX : rect.left + rect.width * 0.5;
        const midRatio = rect.width
          ? Math.max(0, Math.min(1, (clientX - rect.left) / rect.width))
          : 0.5;
        const { startMs, endMs } = activeWindowRef.current;
        const curSpan = Math.max(1000, endMs - startMs);
        pinchRef.current = {
          active: true,
          touchDriven: false,
          initialDist: 100,
          startSpan: curSpan,
          anchorTime: startMs + midRatio * curSpan,
          midRatio,
        };
      }
    };

    const handleGestureChange = (e) => {
      if (e.cancelable) e.preventDefault();
      if (pinchRef.current.touchDriven) return;
      const scaleVal = typeof e.scale === 'number' && e.scale > 0.05 ? e.scale : 1;
      const { minMs, maxMs, minSpanMs } = activeWindowRef.current;
      const totalAvailable = Math.max(60000, maxMs - minMs);
      const nextSpan = Math.max(
        minSpanMs,
        Math.min(totalAvailable, pinchRef.current.startSpan / scaleVal)
      );
      if (nextSpan >= totalAvailable * 0.999) {
        setZoomWindow(null);
        return;
      }
      const midRatio = pinchRef.current.midRatio ?? 0.5;
      let nextStart = pinchRef.current.anchorTime - midRatio * nextSpan;
      let nextEnd = nextStart + nextSpan;
      if (nextStart < minMs) {
        nextStart = minMs;
        nextEnd = minMs + nextSpan;
      }
      if (nextEnd > maxMs) {
        nextEnd = maxMs;
        nextStart = Math.max(minMs, maxMs - nextSpan);
      }
      setZoomWindow({ startMs: nextStart, endMs: nextEnd });
    };

    const handleGestureEnd = (e) => {
      if (e.cancelable) e.preventDefault();
      if (!pinchRef.current.touchDriven) {
        pinchRef.current.active = false;
      }
    };

    targetEl.addEventListener('wheel', handleWheel, { passive: false });
    targetEl.addEventListener('touchstart', handleTouchStart, { passive: false });
    targetEl.addEventListener('touchmove', handleTouchMove, { passive: false });
    targetEl.addEventListener('touchend', handleTouchEnd);
    targetEl.addEventListener('touchcancel', handleTouchEnd);
    targetEl.addEventListener('gesturestart', handleGestureStart, { passive: false });
    targetEl.addEventListener('gesturechange', handleGestureChange, { passive: false });
    targetEl.addEventListener('gestureend', handleGestureEnd, { passive: false });

    return () => {
      targetEl.removeEventListener('wheel', handleWheel);
      targetEl.removeEventListener('touchstart', handleTouchStart);
      targetEl.removeEventListener('touchmove', handleTouchMove);
      targetEl.removeEventListener('touchend', handleTouchEnd);
      targetEl.removeEventListener('touchcancel', handleTouchEnd);
      targetEl.removeEventListener('gesturestart', handleGestureStart);
      targetEl.removeEventListener('gesturechange', handleGestureChange);
      targetEl.removeEventListener('gestureend', handleGestureEnd);
    };
  }, []);

  // Desktop mouse drag panning
  useEffect(() => {
    const onMouseMove = (e) => {
      if (!dragRef.current.active || !svgRef.current) return;
      const dx = e.clientX - dragRef.current.startX;
      if (Math.abs(dx) > 3) {
        dragRef.current.moved = true;
      }
      const rect = svgRef.current.getBoundingClientRect();
      if (!rect.width) return;
      const { minMs, maxMs } = activeWindowRef.current;
      const span = dragRef.current.endMs - dragRef.current.startMs;
      const timeShift = -(dx / rect.width) * span;
      let nextStart = dragRef.current.startMs + timeShift;
      let nextEnd = dragRef.current.endMs + timeShift;
      if (nextStart < minMs) {
        nextStart = minMs;
        nextEnd = minMs + span;
      }
      if (nextEnd > maxMs) {
        nextEnd = maxMs;
        nextStart = Math.max(minMs, maxMs - span);
      }
      setZoomWindow({ startMs: nextStart, endMs: nextEnd });
    };

    const onMouseUp = () => {
      if (dragRef.current.active) {
        dragRef.current.active = false;
        setIsDragging(false);
      }
    };

    window.addEventListener('mousemove', onMouseMove);
    window.addEventListener('mouseup', onMouseUp);
    return () => {
      window.removeEventListener('mousemove', onMouseMove);
      window.removeEventListener('mouseup', onMouseUp);
    };
  }, []);

  const hoverPoint = useMemo(() => {
    if (hoverRatio === null) return null;
    const hoverTimeMs = winStartMs + hoverRatio * (winEndMs - winStartMs);
    // Snap to closest point in visibleSeries if within 1.8% of window width
    let closest = null;
    let minDiff = Infinity;
    for (const pt of visibleSeries) {
      const diff = Math.abs(pt.timeMs - hoverTimeMs);
      if (diff < minDiff) {
        minDiff = diff;
        closest = pt;
      }
    }
    if (closest && minDiff <= (winEndMs - winStartMs) * 0.018) {
      return closest;
    }
    const val = interpolateValueAt(hoverTimeMs);
    return {
      timestamp: new Date(hoverTimeMs).toISOString(),
      timeMs: hoverTimeMs,
      total_equity_usd: val,
      val,
    };
  }, [hoverRatio, winStartMs, winEndMs, visibleSeries]);

  const firstPoint = visibleSeries[0] || chartSeries[0];
  const lastPoint = visibleSeries[visibleSeries.length - 1] || chartSeries[chartSeries.length - 1];

  const displayEquityUsd =
    hoverPoint !== null
      ? Number(hoverPoint.total_equity_usd || 0)
      : zoomWindow
        ? Number(lastPoint?.total_equity_usd || 0)
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
    const padBottom = 22;
    const usableW = width - padX * 2;
    const usableH = height - padTop - padBottom;
    const bottomY = padTop + usableH;

    const values = visibleSeries.map((p) => Math.max(0, Number(p.total_equity_usd || 0)));
    const minVal = 0;
    const peakVal = Math.max(0, ...values);
    const maxVal = peakVal > 0 ? peakVal * 1.16 : 100;
    const span = maxVal - minVal;

    const toY = (val) => {
      const clamped = Math.max(minVal, Math.min(maxVal, Number(val || 0)));
      const normY = span > 1e-4 ? (clamped - minVal) / span : 0;
      return bottomY - normY * usableH;
    };

    const timeSpan = Math.max(1, winEndMs - winStartMs);
    const toX = (tMs) => {
      const ratio = Math.max(0, Math.min(1, (tMs - winStartMs) / timeSpan));
      return padX + ratio * usableW;
    };

    const coords = visibleSeries.map((pt) => {
      const x = toX(pt.timeMs);
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
      { value: 0, y: bottomY },
    ];

    const tickRatios = width < 420 ? [0.18, 0.5, 0.82] : [0.15, 0.38, 0.62, 0.85];
    const showDate = timeSpan > 26 * 3600 * 1000;
    const showSeconds = timeSpan < 10 * 60 * 1000;
    const xTicks = tickRatios.map((r) => {
      const tMs = winStartMs + r * timeSpan;
      const d = new Date(tMs);
      const hh = String(d.getHours()).padStart(2, '0');
      const mm = String(d.getMinutes()).padStart(2, '0');
      const ss = String(d.getSeconds()).padStart(2, '0');
      const label = showDate
        ? `${d.getMonth() + 1}/${d.getDate()} ${hh}:${mm}`
        : showSeconds
          ? `${hh}:${mm}:${ss}`
          : `${hh}:${mm}`;
      return { x: padX + r * usableW, ratio: r, label };
    });

    return { width, height, padX, padTop, bottomY, coords, linePath, areaPath, yTicks, xTicks, toX, toY };
  }, [visibleSeries, containerWidth, winStartMs, winEndMs]);

  const strokeColor = isPositive ? '#3b82f6' : '#f43f5e';
  const hoveredCoord = hoverPoint
    ? {
        x: svgGeometry.toX(hoverPoint.timeMs),
        y: svgGeometry.toY(hoverPoint.total_equity_usd),
        pt: hoverPoint,
      }
    : null;

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
        style={{
          position: 'relative',
          width: '100%',
          height: '195px',
          touchAction: 'none',
          WebkitUserSelect: 'none',
          userSelect: 'none',
        }}
        onMouseLeave={() => setHoverRatio(null)}
      >
        <svg
          ref={svgRef}
          viewBox={`0 0 ${svgGeometry.width} ${svgGeometry.height}`}
          preserveAspectRatio="none"
          style={{
            width: '100%',
            height: '100%',
            overflow: 'visible',
            display: 'block',
            cursor: isDragging ? 'grabbing' : 'crosshair',
            WebkitUserSelect: 'none',
            userSelect: 'none',
            touchAction: 'none',
          }}
          data-testid="equity-area-chart"
          onMouseDown={(e) => {
            if (e.button !== 0) return;
            dragRef.current = {
              active: true,
              moved: false,
              startX: e.clientX,
              startMs: winStartMs,
              endMs: winEndMs,
            };
            setIsDragging(true);
          }}
          onDoubleClick={() => {
            setZoomWindow(null);
            setHoverRatio(null);
          }}
          onMouseMove={(e) => {
            if (dragRef.current.active && dragRef.current.moved) return;
            const rect = e.currentTarget.getBoundingClientRect();
            if (!rect.width || chartSeries.length === 0) return;
            const relX = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
            setHoverRatio(relX);
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

          {/* Vertical X-Axis Time Guide Lines */}
          {svgGeometry.xTicks.map((xt, idx) => (
            <line
              key={`xt-${idx}`}
              x1={xt.x}
              y1={svgGeometry.padTop}
              x2={xt.x}
              y2={svgGeometry.bottomY}
              stroke="rgba(148, 163, 184, 0.08)"
              strokeDasharray="3 3"
              strokeWidth="1"
            />
          ))}

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
              y2={svgGeometry.bottomY}
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

        {/* Crisp X-Axis Time Labels along bottom */}
        {svgGeometry.xTicks.map((xt, idx) => (
          <div
            key={`xt-lbl-${idx}`}
            className="mono"
            style={{
              position: 'absolute',
              left: `${(xt.x / svgGeometry.width) * 100}%`,
              bottom: '1px',
              transform: 'translateX(-50%)',
              fontSize: '10px',
              color: 'rgba(148, 163, 184, 0.72)',
              pointerEvents: 'none',
              whiteSpace: 'nowrap',
            }}
          >
            {xt.label}
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

      {/* Coinbase-Style Timeframe Selector Bar + Zoom Controls */}
      <div
        className="timeframe-tabs"
        role="tablist"
        aria-label="Chart Timeframe"
        style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '8px', flexWrap: 'wrap' }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: '4px', flexWrap: 'wrap' }}>
          {TIMEFRAMES.map((tf) => (
            <button
              key={tf}
              type="button"
              role="tab"
              aria-selected={range === tf}
              data-testid={`timeframe-tab-${tf}`}
              className={`timeframe-tab mono ${range === tf && !zoomWindow ? 'active' : ''}`}
              onClick={() => {
                setRange(tf);
                setZoomWindow(null);
              }}
            >
              {tf}
            </button>
          ))}
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          <button
            type="button"
            className="timeframe-tab mono"
            onClick={() => applyZoomFactor(0.7, 0.5)}
            title={lang === 'ko' ? '확대' : 'Zoom In'}
            style={{ padding: '4px 9px', fontWeight: 700 }}
          >
            +
          </button>
          <button
            type="button"
            className="timeframe-tab mono"
            onClick={() => applyZoomFactor(1.4, 0.5)}
            title={lang === 'ko' ? '축소' : 'Zoom Out'}
            style={{ padding: '4px 9px', fontWeight: 700 }}
          >
            −
          </button>
          {zoomWindow && (
            <button
              type="button"
              className="timeframe-tab mono"
              onClick={() => setZoomWindow(null)}
              style={{ fontSize: '11px', color: 'var(--sky)' }}
            >
              {lang === 'ko' ? '초기화' : 'Reset'}
            </button>
          )}
        </div>
      </div>
    </div>
  );
};
