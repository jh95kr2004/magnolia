import React from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { Dashboard } from '../pages/Dashboard/Dashboard';
import { LanguageProvider } from '../contexts/LanguageContext';
import { MockEventSource } from './setup';

vi.mock('../services/api', () => ({
  apiClient: {
    get: vi.fn().mockResolvedValue({ data: [] }),
    delete: vi.fn().mockResolvedValue({ data: {} })
  }
}));

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ viewMode: 'PAPER', setBotStatuses: vi.fn() }),
  buildSseUrl: (path, mode) => `${path}?mode=${mode}`
}));

vi.mock('../contexts/ToastContext', () => ({
  useToast: () => ({
    showToast: vi.fn(),
    requestConfirm: vi.fn()
  })
}));

describe('Dashboard dynamic multi-exchange balances & positions', () => {
  beforeEach(() => {
    MockEventSource.reset();
    localStorage.setItem('magnolia_lang', 'ko');
  });

  it('dynamically renders N exchange balance cards from SSE portfolio event', async () => {
    render(
      <LanguageProvider>
        <MemoryRouter>
          <Dashboard />
        </MemoryRouter>
      </LanguageProvider>
    );

    const sse = MockEventSource.instances[0];
    expect(sse).toBeDefined();

    await act(async () => {
      sse.emit('portfolio', {
        total_equity_usd: 120000,
        available_balance: 120000,
        foreign_share: 0.6,
        rebalance_trigger_skew: 0.2,
        rebalance_target_skew: 0.5,
        exchange_balances: [
          { name: 'UPBIT', quote_currency: 'KRW', available_native: 65000000, available_usd: 48000, percent: 40 },
          { name: 'COINBASE', quote_currency: 'USD', available_native: 36000, available_usd: 36000, percent: 30 },
          { name: 'BINANCE', quote_currency: 'USDT', available_native: 36000, available_usd: 36000, percent: 30 }
        ]
      });
    });

    expect(screen.getByText('UPBIT 가용 원화 (KRW)')).toBeInTheDocument();
    expect(screen.getByText('COINBASE 가용 달러 (USD)')).toBeInTheDocument();
    expect(screen.getByText('BINANCE 가용 달러 (USDT)')).toBeInTheDocument();
  });

  it('renders active positions received via SSE positions event', async () => {
    render(
      <LanguageProvider>
        <MemoryRouter>
          <Dashboard />
        </MemoryRouter>
      </LanguageProvider>
    );

    // Wait for initial apiClient.get('/positions/active').then(...) chain to settle
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    const sse = MockEventSource.instances[MockEventSource.instances.length - 1];
    await act(async () => {
      sse.emit('positions', {
        mode: 'PAPER',
        positions: [
          {
            PositionID: 'pos-btc-1',
            BaseExchange: 'UPBIT',
            ForeignExchange: 'COINBASE',
            BaseSymbol: 'BTC-KRW',
            ForeignSymbol: 'BTC-USD',
            Type: 'FOREIGN_TRANSFER',
            Status: 'OPEN',
            InvestmentUSD: 5000,
            EntryPremium: 0.035,
            CurrentPnLUSD: 145.5,
            CurrentPnLPct: 2.91
          }
        ]
      });
    });

    expect(screen.getByText('BTC')).toBeInTheDocument();
    expect(screen.getByText('UPBIT → COINBASE')).toBeInTheDocument();
    expect(screen.getAllByText('+$145.50').length).toBeGreaterThanOrEqual(2);
  });
});
