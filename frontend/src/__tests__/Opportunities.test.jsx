import React from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, act, fireEvent } from '@testing-library/react';
import { Opportunities } from '../pages/Opportunities/Opportunities';
import { LanguageProvider } from '../contexts/LanguageContext';
import { MockEventSource } from './setup';
import { apiClient } from '../services/api';

vi.mock('../services/api', () => ({
  apiClient: {
    get: vi.fn().mockResolvedValue({
      data: {
        exchange: 'UPBIT',
        current_offset_pct: 0,
        target_offset_pct: 0,
        ramp_minutes: 0
      }
    }),
    post: vi.fn().mockResolvedValue({
      data: {
        sim_status: {
          exchange: 'UPBIT',
          current_offset_pct: 4.0,
          target_offset_pct: 4.0,
          ramp_minutes: 5
        }
      }
    })
  }
}));

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ viewMode: 'PAPER' }),
  buildSseUrl: (path, mode) => `${path}?mode=${mode}`
}));

vi.mock('../contexts/ToastContext', () => ({
  useToast: () => ({
    showToast: vi.fn(),
    requestConfirm: vi.fn()
  })
}));

describe('Opportunities SSE raw_opportunities & filtered_opportunities rendering', () => {
  beforeEach(() => {
    MockEventSource.reset();
    vi.clearAllMocks();
    localStorage.setItem('magnolia_lang', 'ko');
  });

  it('immediately populates table when raw_opportunities and filtered_opportunities arrive via SSE', async () => {
    render(
      <LanguageProvider>
        <Opportunities />
      </LanguageProvider>
    );

    const oppsSse = MockEventSource.instances.find((x) => x.url.includes('/api/ws/opportunities'));
    expect(oppsSse).toBeDefined();

    await act(async () => {
      oppsSse.emit('raw_opportunities', [
        {
          base_exchange: 'UPBIT',
          foreign_exchange: 'COINBASE',
          base_symbol: 'BTC-KRW',
          foreign_symbol: 'BTC-USD',
          base_price_usd: 92500.0,
          foreign_price_usd: 90000.0,
          premium_pct: 2.78,
          expected_margin: 2.78
        },
        {
          base_exchange: 'UPBIT',
          foreign_exchange: 'COINBASE',
          base_symbol: 'ETH-KRW',
          foreign_symbol: 'ETH-USD',
          base_price_usd: 2350.0,
          foreign_price_usd: 2300.0,
          premium_pct: 2.17,
          expected_margin: 2.17
        }
      ]);

      oppsSse.emit('filtered_opportunities', [
        {
          base_exchange: 'UPBIT',
          foreign_exchange: 'COINBASE',
          base_symbol: 'BTC-KRW',
          foreign_symbol: 'BTC-USD',
          strategy: 'Transfer Loop (C)',
          expected_roi: 0.0256,
          current_premium_pct: 2.78,
          predicted_premium_pct: 0.95,
          target_weight: 0.4
        }
      ]);
    });

    expect(screen.getByText('BTC')).toBeInTheDocument();
    expect(screen.getByText('ETH')).toBeInTheDocument();
    expect(screen.getByText('+2.78%')).toBeInTheDocument();
    expect(screen.getByText('+0.95%')).toBeInTheDocument();
  });

  it('triggers /settings/sim_offset when preset button is clicked', async () => {
    render(
      <LanguageProvider>
        <Opportunities />
      </LanguageProvider>
    );

    const presetBtn = screen.getByText('김프 +3.5% (3분)');
    await act(async () => {
      fireEvent.click(presetBtn);
    });

    expect(apiClient.post).toHaveBeenCalledWith('/settings/sim_offset', {
      exchange: 'UPBIT',
      target_offset_pct: 3.5,
      ramp_minutes: 3
    });
  });
});
