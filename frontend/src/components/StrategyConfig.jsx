import React, { useState } from 'react';

function StrategyConfig() {
  const [strategies, setStrategies] = useState([
    { id: '1', name: 'Spot-to-Spot Arbitrage', enabled: true, minROI: 1.5, stopLoss: 5.0, capLimitUsd: 5000 },
    { id: '2', name: 'Spot-to-Futures Arbitrage', enabled: false, minROI: 2.0, stopLoss: 8.0, capLimitUsd: 10000 },
  ]);

  return (
    <div className="config-container">
      <h2 className="section-title">Risk & Capital Management</h2>
      <div className="strategy-grid">
        {strategies.map((strat, idx) => (
          <div key={strat.id} className="glass-panel strategy-card">
            <div className="card-header">
              <h3>{strat.name}</h3>
              <label className="switch">
                <input type="checkbox" checked={strat.enabled} readOnly />
                <span className="slider round"></span>
              </label>
            </div>
            
            <div className="form-group">
              <label>Target Min. ROI (%)</label>
              <input type="number" defaultValue={strat.minROI} />
            </div>
            
            <div className="form-group">
              <label>Max Stop-Loss (%)</label>
              <input type="number" defaultValue={strat.stopLoss} />
            </div>

            <div className="form-group highlight-group">
              <label>Capital Allocation Limit (USD)</label>
              <input type="number" defaultValue={strat.capLimitUsd} />
            </div>

            <button className="btn-save">Save Configuration</button>
          </div>
        ))}
      </div>
    </div>
  );
}

export default StrategyConfig;
