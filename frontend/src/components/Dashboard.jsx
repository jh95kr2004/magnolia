import React from 'react';

function Dashboard() {
  // Mock data for initial UI setup
  const opportunities = [
    { id: 1, symbol: 'BTC', buyEx: 'Kraken', sellEx: 'Upbit', roi: 1.85, pod: 'node-kr-7x8f2', version: 'v1.0.0' },
    { id: 2, symbol: 'XRP', buyEx: 'Coinbase', sellEx: 'Bithumb', roi: 2.15, pod: 'node-us-9a1c4', version: 'v1.0.0' }
  ];

  return (
    <div className="dashboard-container">
      <div className="stats-row">
        <div className="stat-card glass-panel">
          <h3>Total Assets</h3>
          <p className="stat-value">$24,500.00</p>
        </div>
        <div className="stat-card glass-panel highlight-card">
          <h3>24h Profit</h3>
          <p className="stat-value success">+$415.20</p>
        </div>
        <div className="stat-card glass-panel">
          <h3>Active Positions</h3>
          <p className="stat-value">2</p>
        </div>
      </div>

      <div className="glass-panel table-panel">
        <h3>Live Arbitrage Opportunities</h3>
        <table className="arb-table">
          <thead>
            <tr>
              <th>Asset</th>
              <th>Buy @</th>
              <th>Sell @</th>
              <th>Expected ROI</th>
              <th>Execution Pod</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody>
            {opportunities.map(opp => (
              <tr key={opp.id}>
                <td><strong>{opp.symbol}</strong></td>
                <td>{opp.buyEx}</td>
                <td>{opp.sellEx}</td>
                <td className="success">+{opp.roi}%</td>
                <td>
                  <span className="pod-badge">{opp.pod}</span>
                  <span className="version-badge">{opp.version}</span>
                </td>
                <td><button className="btn-primary">Execute</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export default Dashboard;
