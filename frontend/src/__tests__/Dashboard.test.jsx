import React from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, act, fireEvent } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { Dashboard } from '../pages/Dashboard/Dashboard';
import { LanguageProvider } from '../contexts/LanguageContext';
import { CurrencyProvider } from '../contexts/CurrencyContext';
import { MockEventSource } from './setup';

vi.mock('../services/api', () => ({
  apiClient: {
    get: vi.fn((url) => {
      if (String(url).startsWith('/portfolio/history')) {
        return Promise.resolve({
          data: {
            mode: 'PAPER',
            range: '24H',
            fx_rate_krw: 1400,
            points: [
              { timestamp: '2026-10-29T00:00:00Z', total_equity_usd: 118000, fx_rate_krw: 1400 },
              { timestamp: '2026-10-29T12:00:00Z', total_equity_usd: 120000, fx_rate_krw: 1400 }
            ]
          }
        });
      }
      return Promise.resolve({ data: [] });
    }),
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

describe('Dashboard Equity Chart, Currency Switcher, Exchange Pie/List & Column Tooltips', () => {
  beforeEach(() => {
    MockEventSource.reset();
    localStorage.setItem('magnolia_lang', 'ko');
    localStorage.setItem('magnolia_currency', 'USD');
  });

  it('dynamically renders N exchanges in pie chart and available currency list, and converts USD <-> KRW', async () => {
    render(
      <LanguageProvider>
        <CurrencyProvider>
          <MemoryRouter>
            <Dashboard />
          </MemoryRouter>
        </CurrencyProvider>
      </LanguageProvider>
    );

    const sse = MockEventSource.instances[0];
    expect(sse).toBeDefined();

    await act(async () => {
      sse.emit('portfolio', {
        total_equity_usd: 120000,
        available_balance: 120000,
        fx_rate_krw: 1400,
        foreign_share: 0.6,
        rebalance_trigger_skew: 0.2,
        rebalance_target_skew: 0.5,
        exchange_balances: [
          { name: 'UPBIT', quote_currency: 'KRW', available_native: 67200000, available_usd: 48000, percent: 40 },
          { name: 'COINBASE', quote_currency: 'USD', available_native: 36000, available_usd: 36000, percent: 30 },
          { name: 'BINANCE', quote_currency: 'USDT', available_native: 36000, available_usd: 36000, percent: 30 }
        ]
      });
    });

    expect(screen.getByTestId('equity-area-chart')).toBeInTheDocument();
    expect(screen.getByTestId('exchange-pie-chart')).toBeInTheDocument();
    expect(screen.getByTestId('exchange-row-UPBIT')).toBeInTheDocument();
    expect(screen.getByTestId('exchange-row-COINBASE')).toBeInTheDocument();
    expect(screen.getByTestId('exchange-row-BINANCE')).toBeInTheDocument();

    // Verify USD total equity first
    expect(screen.getByTestId('total-equity-value').textContent).toBe('$120,000.00');

    // Switch currency to KRW (₩) and verify converted valuation (120,000 * 1400 = ₩168,000,000)
    fireEvent.click(screen.getByTestId('currency-btn-KRW'));
    expect(screen.getByTestId('total-equity-value').textContent).toBe('₩168,000,000');
  });

  it('renders active positions and shows column explanation popup on header hover', async () => {
    render(
      <LanguageProvider>
        <CurrencyProvider>
          <MemoryRouter>
            <Dashboard />
          </MemoryRouter>
        </CurrencyProvider>
      </LanguageProvider>
    );

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

    // Hover over the first column header trigger and verify tooltip popup appears
    const triggers = screen.getAllByTestId('column-tooltip-trigger');
    expect(triggers.length).toBeGreaterThanOrEqual(5);
    fireEvent.mouseEnter(triggers[0]);
    expect(screen.getByTestId('column-tooltip-popup')).toBeInTheDocument();
  });
});
