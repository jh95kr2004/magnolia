import React, { useState, useEffect } from 'react';
import { Clock, TrendingUp, TrendingDown, Layers, CheckCircle, RefreshCw, DollarSign, Percent } from 'lucide-react';
import { apiClient } from '../../services/api';
import './History.css';

export const History = () => {
  const [positions, setPositions] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({
    totalTrades: 0,
    totalProfitUsd: 0,
    averageRoi: 0,
    winRate: 0
  });

  const fetchHistory = async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiClient.get('/positions/history');
      if (Array.isArray(res.data)) {
        setPositions(res.data);
        calculateStats(res.data);
      } else {
        setPositions([]);
      }
    } catch (err) {
      setError('Failed to fetch transaction history. Please try again.');
    } finally {
      setLoading(false);
    }
  };

  const calculateStats = (data) => {
    if (data.length === 0) {
      setStats({ totalTrades: 0, totalProfitUsd: 0, averageRoi: 0, winRate: 0 });
      return;
    }

    let totalProfit = 0;
    let profitableTrades = 0;
    let totalRoi = 0;

    data.forEach(p => {
      const pnl = p.RealizedProfitUSD || p.MaxUnrealizedROI || 0;
      totalProfit += pnl;
      if (pnl > 0) {
        profitableTrades++;
      }
      // ROI Calculation based on investment size
      if (p.InvestmentUSD > 0) {
        totalRoi += (pnl / p.InvestmentUSD) * 100;
      }
    });

    setStats({
      totalTrades: data.length,
      totalProfitUsd: totalProfit,
      averageRoi: totalRoi / data.length,
      winRate: (profitableTrades / data.length) * 100
    });
  };

  useEffect(() => {
    fetchHistory();
  }, []);

  return (
    <div className="history-page animate-fade-in delay-100">
      <header className="page-header mb-lg flex-between">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-sm">
            <Clock className="text-accent-primary" /> Trade History
          </h1>
          <p className="text-secondary mt-xs">Completed arbitrage transactions and historical performance metrics.</p>
        </div>
        <button className="btn-secondary flex items-center gap-2" onClick={fetchHistory} disabled={loading}>
          <RefreshCw size={16} className={loading ? 'animate-spin text-accent-primary' : ''} /> Refresh
        </button>
      </header>

      {/* Metrics Row */}
      <div className="history-stats-grid mb-lg">
        <div className="glass-panel stat-card p-lg">
          <div className="flex-between items-center mb-sm">
            <span className="text-sm text-secondary">Total Trades</span>
            <Layers className="text-blue-400" size={20} />
          </div>
          <div className="text-2xl font-bold font-mono">{stats.totalTrades}</div>
        </div>

        <div className="glass-panel stat-card p-lg">
          <div className="flex-between items-center mb-sm">
            <span className="text-sm text-secondary">Total Profit</span>
            <DollarSign className="text-green-400" size={20} />
          </div>
          <div className={`text-2xl font-bold font-mono ${stats.totalProfitUsd >= 0 ? 'text-green-400' : 'text-red-400'}`}>
            ${stats.totalProfitUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
          </div>
        </div>

        <div className="glass-panel stat-card p-lg">
          <div className="flex-between items-center mb-sm">
            <span className="text-sm text-secondary">Average ROI</span>
            <Percent className="text-amber-400" size={20} />
          </div>
          <div className={`text-2xl font-bold font-mono ${stats.averageRoi >= 0 ? 'text-green-400' : 'text-red-400'}`}>
            {stats.averageRoi >= 0 ? '+' : ''}{stats.averageRoi.toFixed(2)}%
          </div>
        </div>

        <div className="glass-panel stat-card p-lg">
          <div className="flex-between items-center mb-sm">
            <span className="text-sm text-secondary">Win Rate</span>
            <CheckCircle className="text-accent-primary" size={20} />
          </div>
          <div className="text-2xl font-bold font-mono">{stats.winRate.toFixed(1)}%</div>
        </div>
      </div>

      {/* Table Container */}
      <div className="glass-panel history-table-container">
        {loading ? (
          <div className="flex-center p-xxl text-secondary gap-md">
            <RefreshCw className="animate-spin text-accent-primary" size={24} /> Loading transaction history...
          </div>
        ) : error ? (
          <div className="p-xl text-center text-red-400">
            {error}
          </div>
        ) : positions.length === 0 ? (
          <div className="p-xxl text-center text-secondary border border-dashed border-white/5 rounded-xl m-lg">
            No completed trades found. Once arbitrage positions are closed, they will appear here.
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="history-table">
              <thead>
                <tr>
                  <th>Position ID</th>
                  <th>Strategy</th>
                  <th>Route</th>
                  <th>Market Leg</th>
                  <th className="text-right">Investment</th>
                  <th className="text-right">Realized PnL</th>
                  <th className="text-center">Opened At</th>
                  <th className="text-center">Status</th>
                </tr>
              </thead>
              <tbody>
                {positions.map(pos => {
                  const pnl = pos.RealizedProfitUSD || pos.MaxUnrealizedROI || 0;
                  const isProfit = pnl >= 0;
                  const roi = pos.InvestmentUSD > 0 ? (pnl / pos.InvestmentUSD) * 100 : 0;

                  return (
                    <tr key={pos.ID || pos.PositionID}>
                      <td className="font-mono text-xs">{pos.PositionID || `POS-${pos.ID}`}</td>
                      <td>
                        <span className="strategy-tag">{pos.Type}</span>
                      </td>
                      <td className="text-sm font-semibold">
                        {pos.BaseExchange} <span className="text-muted">→</span> {pos.ForeignExchange}
                      </td>
                      <td className="font-mono text-xs text-secondary">
                        {pos.BaseSymbol} / {pos.ForeignSymbol}
                      </td>
                      <td className="text-right font-mono text-sm">
                        ${pos.InvestmentUSD.toLocaleString(undefined, { minimumFractionDigits: 2 })}
                      </td>
                      <td className={`text-right font-mono text-sm font-bold ${isProfit ? 'text-green-400' : 'text-red-400'}`}>
                        <div className="flex items-center justify-end gap-xs">
                          {isProfit ? <TrendingUp size={14} /> : <TrendingDown size={14} />}
                          <span>${pnl.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}</span>
                          <span className="text-xs font-normal">({isProfit ? '+' : ''}{roi.toFixed(2)}%)</span>
                        </div>
                      </td>
                      <td className="text-center text-xs text-secondary font-mono">
                        {pos.OpenedAt ? new Date(pos.OpenedAt).toLocaleString() : 'N/A'}
                      </td>
                      <td className="text-center">
                        <span className="status-badge closed">CLOSED</span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
};

export default History;
