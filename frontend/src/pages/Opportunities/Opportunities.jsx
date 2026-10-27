import React, { useState, useEffect } from 'react';
import { RefreshCw, Search, Sliders, Zap, Layers, AlertOctagon } from 'lucide-react';
import { apiClient } from '../../services/api';
import './Opportunities.css';

export const Opportunities = () => {
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState('filtered'); // 'filtered' | 'raw'
  const [filteredOpportunities, setFilteredOpportunities] = useState([]);
  const [rawOpportunities, setRawOpportunities] = useState([]);
  
  // Interactive Filter States
  const [searchTerm, setSearchTerm] = useState('');
  const [minRoi, setMinRoi] = useState(0.0);
  const [maxRisk, setMaxRisk] = useState(100.0);
  const [lastSync, setLastSync] = useState(null);

  useEffect(() => {
    const apiHost = window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1' ? 'http://localhost:8081' : '';
    const oppsSrc = new EventSource(`${apiHost}/api/ws/opportunities`, { withCredentials: true });
    
    // Helper to process and sort raw/filtered opportunities as flat lists
    const processOpportunities = (dataList, isRaw) => {
      if (!Array.isArray(dataList)) return [];
      return dataList
        .filter(o => {
          let currentRoi = isRaw ? o.expected_margin : o.expected_roi;
          return currentRoi > 0;
        })
        .sort((a, b) => {
          let roiA = isRaw ? a.expected_margin : a.expected_roi;
          let roiB = isRaw ? b.expected_margin : b.expected_roi;
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

  const handleForceEnter = async (opp) => {
    const route = `${opp.base_exchange}:${opp.base_symbol.split('-')[0]} ➔ ${opp.foreign_exchange}:${opp.foreign_symbol.split('-')[0]}`;
    const roi = (opp.expected_roi * 100).toFixed(2);
    if (!window.confirm(`Force enter arbitrage for ${route}?\nExpected ROI: ${roi}%`)) return;
    try {
      await apiClient.post('/positions/force_enter', opp);
      alert('Force entry signal routed to Strategy Engine.');
    } catch (err) {
      alert('Failed to enter: ' + (err.response?.data?.error || err.message));
    }
  };

  // Perform Local Search & Filtering on live lists
  const displayFiltered = filteredOpportunities.filter(o => {
    const matchSearch = o.base_symbol.toLowerCase().includes(searchTerm.toLowerCase()) ||
                        o.base_exchange.toLowerCase().includes(searchTerm.toLowerCase()) ||
                        o.foreign_exchange.toLowerCase().includes(searchTerm.toLowerCase());
    const roiPct = o.expected_roi * 100;
    const matchRoi = roiPct >= minRoi;
    const matchRisk = o.risk_score <= maxRisk;
    return matchSearch && matchRoi && matchRisk;
  });

  const displayRaw = rawOpportunities.filter(o => {
    const matchSearch = o.base_symbol.toLowerCase().includes(searchTerm.toLowerCase()) ||
                        o.base_exchange.toLowerCase().includes(searchTerm.toLowerCase()) ||
                        o.foreign_exchange.toLowerCase().includes(searchTerm.toLowerCase());
    const matchRoi = o.expected_margin >= minRoi;
    return matchSearch && matchRoi;
  });

  return (
    <div className="opportunities-page animate-fade-in delay-100">
      <header className="page-header flex-between">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-sm">
            <Sliders className="text-accent-primary" /> Market Opportunities Scanner
          </h1>
          <p className="text-secondary mt-xs">Real-time arbitrage signals across global exchanges with interactive risk control filters.</p>
        </div>
        <div className="flex items-center gap-sm">
          {lastSync && <span className="text-xs text-muted">Sync: {lastSync.toLocaleTimeString()}</span>}
          <button className="btn-secondary flex items-center gap-2">
            <RefreshCw size={16} className={loading ? 'animate-spin text-accent-primary' : ''} /> {loading ? 'Scanning...' : 'Live Scan'}
          </button>
        </div>
      </header>

      {/* Filter Control Panel */}
      <div className="opportunities-filters-panel glass-panel p-xl">
        <div className="opportunities-filters">
          
          <div className="filter-group">
            <label htmlFor="search-input">Search Pair / Exchange</label>
            <div style={{ position: 'relative' }}>
              <input
                id="search-input"
                type="text"
                placeholder="e.g. BTC, Upbit, Coinbase"
                value={searchTerm}
                onChange={e => setSearchTerm(e.target.value)}
                className="filter-input w-full"
                style={{ paddingLeft: '36px' }}
              />
              <Search size={16} className="text-muted" style={{ position: 'absolute', left: '12px', top: '50%', transform: 'translateY(-50%)' }} />
            </div>
          </div>

          <div className="filter-group">
            <label htmlFor="roi-slider">Min Expected ROI: <span className="text-amber-400 font-bold">{minRoi.toFixed(2)}%</span></label>
            <div className="slider-container">
              <input
                id="roi-slider"
                type="range"
                min="0.0"
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
              <label htmlFor="risk-slider">Max Risk Score: <span className="text-red-400 font-bold">{maxRisk.toFixed(0)}</span></label>
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
                setMinRoi(0.0);
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
            <Zap size={14} /> Filtered (Actionable)
          </button>
          <button 
            className={`opportunity-tab ${activeTab === 'raw' ? 'active raw' : ''}`}
            onClick={() => setActiveTab('raw')}
          >
            <Layers size={14} /> Raw (Tier 1 Global)
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
                    <th>Route</th>
                    <th>Risk Score</th>
                    <th>Expected ROI</th>
                    <th className="text-right">Action</th>
                  </tr>
                </thead>
                <tbody>
                  {displayFiltered.length === 0 ? (
                    <tr>
                      <td colSpan={6} className="text-center text-secondary py-xl" style={{ borderBottom: 'none' }}>
                        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 'var(--space-xs)' }}>
                          <AlertOctagon size={20} className="text-muted" />
                          <span>No actionable opportunities matching your current filters or account settings.</span>
                        </div>
                      </td>
                    </tr>
                  ) : (
                    displayFiltered.map((o, idx) => (
                      <tr key={idx} className="opportunity-row">
                        <td>
                          <div className="flex items-center gap-xs font-semibold text-white">
                            <Zap size={14} className="text-amber-500" />
                            {o.strategy}
                          </div>
                        </td>
                        <td>
                          <span className="asset-badge">{o.base_symbol.split('-')[0]}</span>
                        </td>
                        <td>
                          <div className="route-container">
                            <span className="exchange-name base">{o.base_exchange}</span>
                            <span className="route-arrow">→</span>
                            <span className="exchange-name foreign">{o.foreign_exchange}</span>
                          </div>
                        </td>
                        <td>
                          <span className={`risk-badge ${o.risk_score > 30 ? 'high' : 'low'}`}>
                            {o.risk_score?.toFixed(1)}
                          </span>
                        </td>
                        <td>
                          <span className="roi-value positive">
                            {(o.expected_roi * 100).toFixed(2)}%
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
                    ))
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
                    <th>Route</th>
                    <th>Base Price</th>
                    <th>Foreign Price</th>
                    <th>Premium</th>
                    <th>Funding Diff</th>
                    <th>Expected Margin</th>
                  </tr>
                </thead>
                <tbody>
                  {displayRaw.length === 0 ? (
                    <tr>
                      <td colSpan={7} className="text-center text-secondary py-xl" style={{ borderBottom: 'none' }}>
                        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 'var(--space-xs)' }}>
                          <AlertOctagon size={20} className="text-muted" />
                          <span>No global Raw opportunities matching your search or ROI criteria.</span>
                        </div>
                      </td>
                    </tr>
                  ) : (
                    displayRaw.map((o, idx) => (
                      <tr key={idx} className="opportunity-row">
                        <td>
                          <span className="asset-badge raw">{o.base_symbol.split('-')[0]}</span>
                        </td>
                        <td>
                          <div className="route-container">
                            <span className="exchange-name base">{o.base_exchange}</span>
                            <span className="route-arrow">→</span>
                            <span className="exchange-name foreign">{o.foreign_exchange}</span>
                          </div>
                        </td>
                        <td className="font-mono text-secondary">
                          ${o.base_price_usd?.toLocaleString(undefined, {minimumFractionDigits: 2, maximumFractionDigits: 2})}
                        </td>
                        <td className="font-mono text-secondary">
                          ${o.foreign_price_usd?.toLocaleString(undefined, {minimumFractionDigits: 2, maximumFractionDigits: 2})}
                        </td>
                        <td className="font-mono text-secondary">
                          {o.premium_pct?.toFixed(2)}%
                        </td>
                        <td className="font-mono text-secondary">
                          {(o.foreign_funding_rate_pct - o.base_funding_rate_pct)?.toFixed(3)}%
                        </td>
                        <td>
                          <span className="roi-value raw">
                            {o.expected_margin?.toFixed(2)}%
                          </span>
                        </td>
                      </tr>
                    ))
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
