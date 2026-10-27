import React, { useState, useEffect } from 'react';
import { RefreshCw, Search, Sliders, Zap, Layers, AlertOctagon, TrendingUp } from 'lucide-react';
import { apiClient } from '../../services/api';
import { buildSseUrl } from '../../contexts/AuthContext';
import { useToast } from '../../contexts/ToastContext';
import './Opportunities.css';

export const Opportunities = () => {
  const { showToast, requestConfirm } = useToast();
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState('filtered'); // 'filtered' | 'raw'
  const [filteredOpportunities, setFilteredOpportunities] = useState([]);
  const [rawOpportunities, setRawOpportunities] = useState([]);

  // Interactive Filter States
  const [searchTerm, setSearchTerm] = useState('');
  const [minRoi, setMinRoi] = useState(-2.0);
  const [maxRisk, setMaxRisk] = useState(100.0);
  const [lastSync, setLastSync] = useState(null);

  useEffect(() => {
    const oppsSrc = new EventSource(buildSseUrl('/api/ws/opportunities'), { withCredentials: true });

    const processOpportunities = (dataList, isRaw) => {
      if (!Array.isArray(dataList)) return [];
      return [...dataList].sort((a, b) => {
        const roiA = isRaw ? (a.expected_margin || 0) : (a.expected_roi || 0);
        const roiB = isRaw ? (b.expected_margin || 0) : (b.expected_roi || 0);
        return roiB - roiA;
      });
    };

    oppsSrc.addEventListener('filtered_opportunities', (e) => {
      try {
        const data = JSON.parse(e.data);
        setFilteredOpportunities(processOpportunities(data, false));
        setLastSync(new Date());
        setLoading(false);
      } catch (err) {}
    });

    oppsSrc.addEventListener('raw_opportunities', (e) => {
      try {
        const data = JSON.parse(e.data);
        setRawOpportunities(processOpportunities(data, true));
        setLastSync(new Date());
        setLoading(false);
      } catch (err) {}
    });

    return () => {
      oppsSrc.close();
    };
  }, []);

  const handleForceEnter = (opp) => {
    const asset = opp.base_symbol?.split('-')[0] || opp.base_symbol;
    const route = `${opp.base_exchange}:${asset} ➔ ${opp.foreign_exchange}`;
    const roi = ((opp.expected_roi || 0) * 100).toFixed(2);
    const weightPct = ((opp.target_weight || 0.2) * 100).toFixed(1);

    requestConfirm({
      title: `Force Enter Arbitrage (${asset})`,
      message: `Route: ${route}\nStrategy: ${opp.strategy}\nExpected Net ROI: +${roi}%\nAI Target Allocation Weight: ${weightPct}%\n\nConfirm real order execution via User Engine?`,
      confirmText: 'Execute Hedge Entry',
      danger: false,
      onConfirm: async () => {
        try {
          await apiClient.post('/positions/force_enter', opp);
          showToast(`Force entry signal routed to Strategy Engine for ${route}.`, 'success');
        } catch (err) {
          showToast('Failed to enter: ' + (err.response?.data?.error || err.message), 'error');
        }
      }
    });
  };

  // Perform Local Search & Filtering on live lists
  const displayFiltered = filteredOpportunities.filter(o => {
    const matchSearch =
      (o.base_symbol || '').toLowerCase().includes(searchTerm.toLowerCase()) ||
      (o.base_exchange || '').toLowerCase().includes(searchTerm.toLowerCase()) ||
      (o.foreign_exchange || '').toLowerCase().includes(searchTerm.toLowerCase()) ||
      (o.strategy || '').toLowerCase().includes(searchTerm.toLowerCase());
    const roiPct = (o.expected_roi || 0) * 100;
    const matchRoi = roiPct >= minRoi;
    const matchRisk = (o.risk_score || 0) <= maxRisk;
    return matchSearch && matchRoi && matchRisk;
  });

  const displayRaw = rawOpportunities.filter(o => {
    const matchSearch =
      (o.base_symbol || '').toLowerCase().includes(searchTerm.toLowerCase()) ||
      (o.base_exchange || '').toLowerCase().includes(searchTerm.toLowerCase()) ||
      (o.foreign_exchange || '').toLowerCase().includes(searchTerm.toLowerCase());
    const matchRoi = (o.expected_margin || 0) >= minRoi;
    return matchSearch && matchRoi;
  });

  return (
    <div className="opportunities-page animate-fade-in delay-100">
      <header className="page-header flex-between">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-sm">
            <Sliders className="text-accent-primary" /> Global Arbitrage Scanner
          </h1>
          <p className="text-secondary mt-xs">
            Live cross-exchange spread telemetry, 8-hour AI Transformer forecasts, and RL portfolio target weights.
          </p>
        </div>
        <div className="flex items-center gap-sm">
          {lastSync && <span className="text-xs text-muted font-mono">Sync: {lastSync.toLocaleTimeString()}</span>}
          <button className="btn-secondary flex items-center gap-2">
            <RefreshCw size={16} className={loading ? 'animate-spin text-accent-primary' : ''} />
            {loading ? 'Scanning...' : 'Live Stream Active'}
          </button>
        </div>
      </header>

      {/* Filter Control Panel */}
      <div className="opportunities-filters-panel glass-panel p-xl">
        <div className="opportunities-filters">
          <div className="filter-group">
            <label htmlFor="search-input">Search Asset / Exchange / Strategy</label>
            <div style={{ position: 'relative' }}>
              <input
                id="search-input"
                type="text"
                placeholder="e.g. BTC, Upbit, Coinbase, Strategy C"
                value={searchTerm}
                onChange={e => setSearchTerm(e.target.value)}
                className="filter-input w-full"
                style={{ paddingLeft: '36px' }}
              />
              <Search size={16} className="text-muted" style={{ position: 'absolute', left: '12px', top: '50%', transform: 'translateY(-50%)' }} />
            </div>
          </div>

          <div className="filter-group">
            <label htmlFor="roi-slider">
              Min Expected ROI / Margin: <span className="text-amber-400 font-bold">{minRoi.toFixed(1)}%</span>
            </label>
            <div className="slider-container">
              <input
                id="roi-slider"
                type="range"
                min="-5.0"
                max="5.0"
                step="0.1"
                value={minRoi}
                onChange={e => setMinRoi(parseFloat(e.target.value))}
                className="filter-slider"
              />
              <span className="slider-val">{minRoi.toFixed(1)}%</span>
            </div>
          </div>

          {activeTab === 'filtered' && (
            <div className="filter-group">
              <label htmlFor="risk-slider">
                Max AI Risk Score: <span className="text-red-400 font-bold">{maxRisk.toFixed(0)}</span>
              </label>
              <div className="slider-container">
                <input
                  id="risk-slider"
                  type="range"
                  min="10"
                  max="100"
                  step="5"
                  value={maxRisk}
                  onChange={e => setMaxRisk(parseFloat(e.target.value))}
                  className="filter-slider"
                />
                <span className="slider-val">{maxRisk.toFixed(0)}</span>
              </div>
            </div>
          )}

          <div className="filter-group action-group">
            <button
              className="btn-secondary"
              onClick={() => {
                setSearchTerm('');
                setMinRoi(-2.0);
                setMaxRisk(100.0);
              }}
            >
              Reset Filters
            </button>
          </div>
        </div>
      </div>

      {/* Main Scanner Section */}
      <div className="scanner-container">
        {/* Tab Navigation */}
        <div className="opportunity-tabs-container">
          <button
            className={`opportunity-tab ${activeTab === 'filtered' ? 'active filtered' : ''}`}
            onClick={() => setActiveTab('filtered')}
          >
            <Zap size={14} /> AI Filtered & Sized ({displayFiltered.length})
          </button>
          <button
            className={`opportunity-tab ${activeTab === 'raw' ? 'active raw' : ''}`}
            onClick={() => setActiveTab('raw')}
          >
            <Layers size={14} /> Raw Global Spreads ({displayRaw.length})
          </button>
        </div>

        {/* Tab Contents */}
        <div className="opportunities-list flex flex-col gap-sm">
          {activeTab === 'filtered' ? (
            <div className="opportunities-table-container">
              <table className="opportunities-table">
                <thead>
                  <tr>
                    <th>Strategy</th>
                    <th>Asset</th>
                    <th>Hedge Route (Short → Long)</th>
                    <th>Current → 8h Pred Spread</th>
                    <th>AI Target Weight</th>
                    <th>Risk Score</th>
                    <th>Expected Net ROI</th>
                    <th className="text-right">Action</th>
                  </tr>
                </thead>
                <tbody>
                  {displayFiltered.length === 0 ? (
                    <tr>
                      <td colSpan={8} className="text-center text-secondary py-xl" style={{ borderBottom: 'none' }}>
                        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 'var(--space-xs)' }}>
                          <AlertOctagon size={20} className="text-muted" />
                          <span>No AI-filtered opportunities matching your current filter criteria.</span>
                        </div>
                      </td>
                    </tr>
                  ) : (
                    displayFiltered.map((o, idx) => {
                      const currPrem = typeof o.current_premium_pct === 'number' ? o.current_premium_pct : 0;
                      const predPrem = typeof o.predicted_premium_pct === 'number' ? o.predicted_premium_pct : currPrem;
                      const weightPct = ((o.target_weight || 0) * 100).toFixed(1);
                      const invUsd = o.investment_usd || 0;

                      return (
                        <tr key={idx} className="opportunity-row">
                          <td>
                            <div className="flex items-center gap-xs font-semibold text-white">
                              <Zap size={14} className="text-amber-500" />
                              {o.strategy}
                            </div>
                          </td>
                          <td>
                            <span className="asset-badge">{(o.base_symbol || '').split('-')[0]}</span>
                          </td>
                          <td>
                            <div className="route-container">
                              <span className="exchange-name base">{o.base_exchange}</span>
                              <span className="route-arrow">→</span>
                              <span className="exchange-name foreign">{o.foreign_exchange}</span>
                            </div>
                          </td>
                          <td className="font-mono text-xs">
                            <span className="text-secondary">{currPrem >= 0 ? '+' : ''}{currPrem.toFixed(2)}%</span>
                            <span className="text-muted mx-1">→</span>
                            <span className="text-amber-300 font-bold">{predPrem >= 0 ? '+' : ''}{predPrem.toFixed(2)}%</span>
                          </td>
                          <td className="font-mono text-xs">
                            <span className="text-blue-300 font-bold">{weightPct}%</span>
                            {invUsd > 0 && (
                              <span className="text-muted ml-1">(${invUsd.toLocaleString(undefined, { maximumFractionDigits: 0 })})</span>
                            )}
                          </td>
                          <td>
                            <span className={`risk-badge ${o.risk_score > 30 ? 'high' : 'low'}`}>
                              {o.risk_score?.toFixed(1)}
                            </span>
                          </td>
                          <td>
                            <span className="roi-value positive">
                              {((o.expected_roi || 0) * 100) >= 0 ? '+' : ''}{((o.expected_roi || 0) * 100).toFixed(2)}%
                            </span>
                          </td>
                          <td className="text-right">
                            <button
                              className="btn-enter"
                              onClick={() => handleForceEnter(o)}
                            >
                              Force Enter
                            </button>
                          </td>
                        </tr>
                      );
                    })
                  )}
                </tbody>
              </table>
            </div>
          ) : (
            <div className="opportunities-table-container">
              <table className="opportunities-table">
                <thead>
                  <tr>
                    <th>Asset</th>
                    <th>Canonical Route</th>
                    <th>Base Price (USD)</th>
                    <th>Foreign Price (USD)</th>
                    <th>Spread (Premium)</th>
                    <th>8h Funding Diff</th>
                    <th>Net Margin</th>
                  </tr>
                </thead>
                <tbody>
                  {displayRaw.length === 0 ? (
                    <tr>
                      <td colSpan={7} className="text-center text-secondary py-xl" style={{ borderBottom: 'none' }}>
                        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 'var(--space-xs)' }}>
                          <AlertOctagon size={20} className="text-muted" />
                          <span>No global Raw opportunities matching your search or margin criteria.</span>
                        </div>
                      </td>
                    </tr>
                  ) : (
                    displayRaw.map((o, idx) => {
                      const prem = o.premium_pct || 0;
                      const margin = o.expected_margin || 0;
                      return (
                        <tr key={idx} className="opportunity-row">
                          <td>
                            <span className="asset-badge raw">{(o.base_symbol || '').split('-')[0]}</span>
                          </td>
                          <td>
                            <div className="route-container">
                              <span className="exchange-name base">{o.base_exchange}</span>
                              <span className="route-arrow">→</span>
                              <span className="exchange-name foreign">{o.foreign_exchange}</span>
                            </div>
                          </td>
                          <td className="font-mono text-secondary">
                            ${o.base_price_usd?.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 4 })}
                          </td>
                          <td className="font-mono text-secondary">
                            ${o.foreign_price_usd?.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 4 })}
                          </td>
                          <td className={`font-mono font-semibold ${prem >= 0 ? 'text-green-400' : 'text-purple-400'}`}>
                            {prem >= 0 ? '+' : ''}{prem.toFixed(2)}%
                            {prem <= 0 && <span className="text-xs text-muted ml-1">(Rev)</span>}
                          </td>
                          <td className="font-mono text-secondary">
                            {((o.foreign_funding_rate_pct || 0) - (o.base_funding_rate_pct || 0)).toFixed(3)}%
                          </td>
                          <td>
                            <span className="roi-value raw">
                              {margin >= 0 ? '+' : ''}{margin.toFixed(2)}%
                            </span>
                          </td>
                        </tr>
                      );
                    })
                  )}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </div>
  );
};
