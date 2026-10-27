import React, { useState, useEffect } from 'react';
import { RefreshCw, Activity, PieChart, TrendingUp, AlertOctagon, Zap, ChevronDown, ChevronRight, Layers } from 'lucide-react';
import { apiClient } from '../../services/api';
import './Dashboard.css';

export const Dashboard = () => {
  const [loading, setLoading] = useState(true);
  const [portfolio, setPortfolio] = useState({ totalUsd: 0, exchanges: [] });
  const [activePositions, setActivePositions] = useState([]);
  
  // Opportunities State
  const [activeTab, setActiveTab] = useState('filtered'); // 'filtered' | 'raw'
  const [filteredOpportunities, setFilteredOpportunities] = useState([]);
  const [rawOpportunities, setRawOpportunities] = useState([]);
  
  const [lastSync, setLastSync] = useState(null);

  useEffect(() => {
    const apiHost = window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1' ? 'http://localhost:8081' : '';
    // 1. Dashboard Core Stream (Portfolio & Positions)
    const dashboardSrc = new EventSource(`${apiHost}/api/ws/dashboard`, { withCredentials: true });
    
    dashboardSrc.addEventListener('portfolio', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data && data.portfolio) {
           // Mock portfolio reduction logic: sum up total USD
           const total = data.portfolio.reduce((acc, b) => acc + (b.available * 100000), 0); // Mock 1 BTC = $100k
           setPortfolio({
             totalUsd: total,
             exchanges: data.portfolio.map((b, i) => ({
               name: i === 0 ? 'UPBIT' : 'COINBASE',
               usdValue: b.available * 100000,
               percent: 50 // Mock equal distribution
             }))
           });
           setLastSync(new Date());
           setLoading(false);
        }
      } catch (err) {}
    });

    dashboardSrc.addEventListener('positions', (e) => {
      try {
        const data = JSON.parse(e.data);
        if (data && data.positions) {
          setActivePositions(data.positions.map(p => ({
            id: p.PositionID || p.ID,
            strategy: p.Type,
            pair: `${p.BaseSymbol} -> ${p.ForeignSymbol}`,
            investedUsd: p.InvestmentUSD || 0,
            currentPnlUsd: p.MaxUnrealizedROI || 0, // Mock usage
            currentPnlPct: 0.0,
            status: p.Status
          })));
        }
      } catch (err) {}
    });

    // 2. High-Frequency Opportunities Stream
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
      } catch (err) {}
    });

    oppsSrc.addEventListener('raw_opportunities', (e) => {
      try {
        const data = JSON.parse(e.data);
        setRawOpportunities(processOpportunities(data, true));
      } catch (err) {}
    });

    return () => {
      dashboardSrc.close();
      oppsSrc.close();
    };
  }, []);

  const handleForceClose = async (positionId) => {
    if (!window.confirm(`Are you sure you want to force close position ${positionId}? This may result in slippage or unbalanced exposure.`)) return;
    try {
      await apiClient.delete(`/positions/active/${positionId}`);
      alert('Force close signal sent to Execution layer.');
    } catch (err) {
      alert('Failed to close: ' + (err.response?.data?.error || err.message));
    }
  };

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

  return (
    <div className="dashboard-page animate-fade-in delay-100">
      <header className="page-header mb-lg flex-between">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-sm">
            <Activity className="text-accent-primary" /> Trading Dashboard
          </h1>
          <p className="text-secondary mt-xs">Real-time portfolio monitoring and algorithmic execution state.</p>
        </div>
        <div className="flex items-center gap-sm">
          {lastSync && <span className="text-xs text-muted">Sync: {lastSync.toLocaleTimeString()}</span>}
        </div>
      </header>

      <div className="dashboard-grid">
        
        {/* Left Column */}
        <div className="dashboard-col-left flex flex-col gap-lg">
          
          {/* Portfolio Breakdown */}
          <div className="dashboard-card glass-panel p-xl">
            <h2 className="text-lg font-semibold mb-md flex items-center gap-sm">
              <PieChart size={20} className="text-blue-400" /> Portfolio Snapshot
            </h2>
            <div className="text-3xl font-bold mb-md">${portfolio.totalUsd.toLocaleString(undefined, {minimumFractionDigits: 2})}</div>
            <div className="flex flex-col gap-sm mt-md">
              {portfolio.exchanges.map(ex => (
                <div key={ex.name} className="flex-between p-sm rounded bg-black/20 border border-white/5">
                  <span className="font-semibold text-sm">{ex.name}</span>
                  <div className="flex items-center gap-md text-sm">
                    <span className="text-secondary">${ex.usdValue.toLocaleString()}</span>
                    <span className="text-blue-300 bg-blue-500/10 px-2 py-1 rounded text-xs">{ex.percent}%</span>
                  </div>
                </div>
              ))}
            </div>
          </div>

          {/* Active Positions */}
          <div className="dashboard-card glass-panel p-xl">
            <h2 className="text-lg font-semibold mb-md flex items-center gap-sm">
              <TrendingUp size={20} className="text-green-400" /> Active Positions
            </h2>
            
            {activePositions.length === 0 ? (
              <div className="p-xl text-center text-sm text-secondary border border-dashed border-white/10 rounded-xl">
                No active arbitrage positions natively managed by Magnolia engine.
              </div>
            ) : (
              <div className="flex flex-col gap-md">
                {activePositions.map(pos => (
                  <div key={pos.id} className="p-md rounded-xl bg-gradient-to-br from-green-500/10 to-transparent border border-green-500/20">
                    <div className="flex-between mb-sm">
                      <span className="text-xs font-bold text-green-400 tracking-wider uppercase px-2 py-1 bg-green-500/10 rounded">{pos.status}</span>
                      <span className="text-xs text-secondary font-mono">{pos.id}</span>
                    </div>
                    <div className="font-semibold mb-xs">{pos.strategy}</div>
                    <div className="text-sm text-secondary mb-md">{pos.pair}</div>
                    <div className="flex-between items-end">
                      <div>
                        <div className="text-xs text-muted mb-1">Invested</div>
                        <div className="font-mono text-sm">${pos.investedUsd.toLocaleString()}</div>
                      </div>
                      <div className="text-right">
                        <div className="text-xs text-muted mb-1">Unrealized PnL</div>
                        <div className={`font-mono font-bold ${pos.currentPnlUsd >= 0 ? 'text-green-400' : 'text-red-400'}`}>
                          {pos.currentPnlUsd >= 0 ? '+' : ''}{pos.currentPnlUsd.toLocaleString()} ({pos.currentPnlPct}%)
                        </div>
                      </div>
                    </div>
                    <div className="mt-md pt-md border-t border-white/10 flex justify-end">
                       <button 
                         className="text-xs flex items-center gap-1 text-red-400 hover:text-red-300 px-3 py-1.5 rounded hover:bg-red-500/10 transition-colors"
                         onClick={() => handleForceClose(pos.id)}
                       >
                         <AlertOctagon size={14} /> Force Close
                       </button>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>

        {/* Right Column: Opportunities Scanner */}
        <div className="dashboard-col-right dashboard-card glass-panel p-xl">
          <div className="flex-between mb-md">
            <h2 className="text-lg font-semibold flex items-center gap-sm mt-0 mb-0">
              <Zap size={20} className="text-amber-400" /> Live Market Opportunities
            </h2>
          </div>
          
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

          <div className="opportunities-list flex flex-col gap-sm overflow-y-auto max-h-[600px] pr-2">
            {activeTab === 'filtered' ? (
              filteredOpportunities.length === 0 ? (
                <div className="p-xl text-center text-sm text-secondary border border-dashed border-white/10 rounded-xl mt-4">
                  No overlapping cross-market pairs found for your account filters.
                </div>
              ) : (
                <div className="opportunities-table-container">
                  <table className="opportunities-table">
                    <thead>
                      <tr>
                        <th>Strategy</th>
                        <th>Asset</th>
                        <th>Route</th>
                        <th>Risk</th>
                        <th>Expected ROI</th>
                        <th className="text-right">Action</th>
                      </tr>
                    </thead>
                    <tbody>
                      {filteredOpportunities.map((o, idx) => (
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
                      ))}
                    </tbody>
                  </table>
                </div>
              )
            ) : (
              rawOpportunities.length === 0 ? (
                <div className="p-xl text-center text-sm text-secondary border border-dashed border-white/10 rounded-xl mt-4">
                  No overlapping cross-market pairs found for Tier 1.
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
                      {rawOpportunities.map((o, idx) => (
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
                      ))}
                    </tbody>
                  </table>
                </div>
              )
            )}
          </div>
        </div>

      </div>
    </div>
  );
};
