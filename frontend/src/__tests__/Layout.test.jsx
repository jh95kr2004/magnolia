import React from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { Layout } from '../components/layout/Layout';
import { LanguageProvider } from '../contexts/LanguageContext';

const mockSwitchViewMode = vi.fn();
const mockSetBotStatuses = vi.fn();
const mockLogout = vi.fn();

let mockAuthState = {
  viewMode: 'PAPER',
  switchViewMode: mockSwitchViewMode,
  botStatuses: { PAPER: 'RUNNING', LIVE: 'STOPPED' },
  setBotStatuses: mockSetBotStatuses,
  logout: mockLogout
};

vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => mockAuthState
}));

vi.mock('../services/api', () => ({
  apiClient: {
    get: vi.fn().mockResolvedValue({ data: [{ PositionID: 'pos-1' }] }),
    post: vi.fn().mockResolvedValue({ data: { paper_bot_status: 'DRAINING', live_bot_status: 'STOPPED' } })
  }
}));

vi.mock('../contexts/ToastContext', () => ({
  useToast: () => ({
    showToast: vi.fn(),
    requestConfirm: vi.fn()
  })
}));

describe('Layout Coinbase-style left sidebar, mobile breadcrumb & global controls', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.setItem('magnolia_lang', 'ko');
    mockAuthState = {
      viewMode: 'PAPER',
      switchViewMode: mockSwitchViewMode,
      botStatuses: { PAPER: 'RUNNING', LIVE: 'STOPPED' },
      setBotStatuses: mockSetBotStatuses,
      logout: mockLogout
    };
  });

  it('renders left sidebar with 4 navigation items, opens mobile breadcrumb menu, and switches language between KO and EN', () => {
    render(
      <LanguageProvider>
        <MemoryRouter initialEntries={['/dashboard']}>
          <Layout>
            <div>Dashboard Content</div>
          </Layout>
        </MemoryRouter>
      </LanguageProvider>
    );

    expect(screen.getByTestId('left-sidebar')).toBeInTheDocument();
    expect(screen.getAllByText('대시보드').length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText('마켓 & 시뮬레이터')).toBeInTheDocument();
    expect(screen.getByText('거래 이력')).toBeInTheDocument();
    expect(screen.getByText('설정')).toBeInTheDocument();

    // Open mobile breadcrumb dropdown
    const bcTrigger = screen.getByTestId('mobile-breadcrumb-trigger');
    fireEvent.click(bcTrigger);
    expect(screen.getByTestId('mobile-breadcrumb-menu')).toBeInTheDocument();

    // Switch language to EN
    fireEvent.click(screen.getByText('KO'));
    expect(screen.getAllByText('Dashboard').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('Market & Simulator').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('Trade History').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('Settings').length).toBeGreaterThanOrEqual(1);
  });

  it('opens 3-option Drain/Stop confirmation modal when stopping bot with open positions', async () => {
    render(
      <LanguageProvider>
        <MemoryRouter initialEntries={['/dashboard']}>
          <Layout>
            <div>Content</div>
          </Layout>
        </MemoryRouter>
      </LanguageProvider>
    );

    const stopBtn = screen.getByText('자동매매 중지');
    fireEvent.click(stopBtn);

    await waitFor(() => {
      expect(screen.getByText('보유 중인 포지션 처리 방식을 선택해 주세요')).toBeInTheDocument();
      expect(screen.getByText('신규 진입만 중단 (기존 포지션은 익절/청산 시까지 자동 관리)')).toBeInTheDocument();
      expect(screen.getByText('즉시 모든 포지션 시장가 청산 후 봇 완전 정지')).toBeInTheDocument();
    });
  });
});
