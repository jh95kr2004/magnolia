import React from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { Settings } from '../pages/Settings/Settings';
import { LanguageProvider } from '../contexts/LanguageContext';

vi.mock('../services/api', () => ({
  apiClient: {
    get: vi.fn((url) => {
      if (url === '/settings/parameters') {
        return Promise.resolve({
          data: {
            MinTargetProfit: 1.5,
            MaxStopLoss: -5.0,
            CapitalLimitUsd: 50000,
            MaxAcceptableSpread: 0.5,
            RebalanceTriggerSkew: 0.2,
            RebalanceTargetSkew: 0.5,
            RebalanceMaxPremium: 0.0
          }
        });
      }
      if (url === '/settings/profile') {
        return Promise.resolve({
          data: {
            paper_upbit_krw: 50000000,
            paper_coinbase_usd: 35000
          }
        });
      }
      return Promise.resolve({ data: [] });
    }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} })
  }
}));

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({
    viewMode: 'PAPER',
    refreshBotStatus: vi.fn()
  })
}));

vi.mock('../contexts/ToastContext', () => ({
  useToast: () => ({
    showToast: vi.fn(),
    requestConfirm: vi.fn()
  })
}));

describe('Settings non-zero default risk parameters & tabs', () => {
  beforeEach(() => {
    localStorage.setItem('magnolia_lang', 'ko');
  });

  it('displays non-zero seeded default parameters in the Risk & Strategy sub-tab', async () => {
    render(
      <LanguageProvider>
        <MemoryRouter initialEntries={['/settings?tab=risk']}>
          <Settings />
        </MemoryRouter>
      </LanguageProvider>
    );

    await waitFor(() => {
      expect(screen.getByDisplayValue('1.5')).toBeInTheDocument();
      expect(screen.getByDisplayValue('-5')).toBeInTheDocument();
      expect(screen.getByDisplayValue('50000')).toBeInTheDocument();
      expect(screen.getByDisplayValue('20')).toBeInTheDocument();
      expect(screen.getByDisplayValue('50')).toBeInTheDocument();
    });
  });
});
