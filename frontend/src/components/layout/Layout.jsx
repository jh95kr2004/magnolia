import React, { useState, useEffect, useRef } from 'react';
import { Outlet, NavLink, useLocation, useNavigate } from 'react-router-dom';
import {
  Play,
  Square,
  Globe,
  LogOut,
  LayoutDashboard,
  TrendingUp,
  History,
  Settings as SettingsIcon,
  ChevronRight,
  ChevronDown,
} from 'lucide-react';
import { useAuth } from '../../contexts/AuthContext';
import { useI18n } from '../../contexts/LanguageContext';
import { useToast } from '../../contexts/ToastContext';
import { apiClient } from '../../services/api';

export const Layout = () => {
  const location = useLocation();
  const navigate = useNavigate();
  const { viewMode, switchViewMode, botStatuses, setBotStatuses, logout } = useAuth();
  const { lang, toggleLanguage, t } = useI18n();
  const { showToast } = useToast();

  const [showStopModal, setShowStopModal] = useState(false);
  const [updatingBot, setUpdatingBot] = useState(false);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);
  const breadcrumbRef = useRef(null);

  const currentBotStatus = botStatuses[viewMode] || (viewMode === 'PAPER' ? 'RUNNING' : 'STOPPED');
  const isRunning = currentBotStatus === 'RUNNING';
  const isDraining = currentBotStatus === 'DRAINING';

  useEffect(() => {
    setMobileMenuOpen(false);
  }, [location.pathname, location.search]);

  useEffect(() => {
    if (!mobileMenuOpen) return;
    const handleOutside = (e) => {
      if (breadcrumbRef.current && !breadcrumbRef.current.contains(e.target)) {
        setMobileMenuOpen(false);
      }
    };
    document.addEventListener('mousedown', handleOutside);
    return () => document.removeEventListener('mousedown', handleOutside);
  }, [mobileMenuOpen]);

  const executeBotCommand = async (subAction) => {
    setUpdatingBot(true);
    try {
      const res = await apiClient.post('/settings/bot_status', {
        mode: viewMode,
        sub_action: subAction,
      });
      if (res.data) {
        setBotStatuses({
          PAPER: res.data.paper_bot_status || botStatuses.PAPER,
          LIVE: res.data.live_bot_status || botStatuses.LIVE,
        });
      }
      setShowStopModal(false);
      showToast(
        lang === 'ko'
          ? `${viewMode === 'PAPER' ? '모의매매' : '실매매'} 상태가 변경되었습니다.`
          : `${viewMode} bot status updated.`,
        'success'
      );
    } catch (err) {
      showToast(err.response?.data?.error || err.message, 'error');
    } finally {
      setUpdatingBot(false);
    }
  };

  const handleBotButtonClick = async () => {
    if (!isRunning) {
      await executeBotCommand('RUN');
      return;
    }
    try {
      const res = await apiClient.get(`/positions/active?mode=${viewMode}`);
      const list = Array.isArray(res.data) ? res.data : [];
      if (list.length > 0) {
        setShowStopModal(true);
      } else {
        await executeBotCommand('STOP_AND_CLOSE');
      }
    } catch {
      await executeBotCommand('DRAIN');
    }
  };

  const searchParams = new URLSearchParams(location.search);
  const currentTab = searchParams.get('tab');

  const isDashboardActive = location.pathname.startsWith('/dashboard') || location.pathname === '/';
  const isMarketActive = location.pathname.startsWith('/opportunities');
  const isHistoryActive =
    location.pathname.startsWith('/history') ||
    (location.pathname.startsWith('/settings') && currentTab === 'history');
  const isSettingsActive =
    location.pathname.startsWith('/settings') && currentTab !== 'history';

  const navItems = [
    {
      key: 'dashboard',
      to: '/dashboard',
      label: t.nav.dashboard,
      subLabel: viewMode === 'PAPER' ? t.nav.paperMode : t.nav.liveMode,
      icon: LayoutDashboard,
      active: isDashboardActive,
    },
    {
      key: 'market',
      to: '/opportunities',
      label: t.nav.market,
      subLabel: lang === 'ko' ? '실시간 김프' : 'Live Spread',
      icon: TrendingUp,
      active: isMarketActive,
    },
    {
      key: 'history',
      to: '/settings?tab=history',
      label: t.nav.history || (lang === 'ko' ? '거래 이력' : 'Trade History'),
      subLabel: lang === 'ko' ? '청산 내역' : 'Closed Trades',
      icon: History,
      active: isHistoryActive,
    },
    {
      key: 'settings',
      to: '/settings',
      label: t.nav.settings,
      subLabel: lang === 'ko' ? 'API & 리스크' : 'API & Risk',
      icon: SettingsIcon,
      active: isSettingsActive,
    },
  ];

  const activeNavItem = navItems.find((item) => item.active) || navItems[0];

  return (
    <div className="app-shell">
      {/* Desktop Left Sidebar Navigation (Coinbase Style) */}
      <aside className="left-sidebar" data-testid="left-sidebar">
        <div
          className="sidebar-brand"
          style={{ cursor: 'pointer' }}
          onClick={() => navigate('/dashboard')}
        >
          <span className="brand-dot" />
          <span>Magnolia</span>
        </div>

        <nav className="sidebar-nav" aria-label="Main Navigation">
          {navItems.map((item) => {
            const Icon = item.icon;
            return (
              <NavLink
                key={item.key}
                to={item.to}
                className={() => `sidebar-nav-item ${item.active ? 'active' : ''}`}
              >
                <Icon size={18} />
                <span>{item.label}</span>
              </NavLink>
            );
          })}
        </nav>

        <div className="sidebar-footer">
          <div className="sidebar-mode-badge">
            <span
              className={`status-dot ${
                isRunning ? 'running' : isDraining ? 'draining' : 'stopped'
              }`}
            />
            <span>
              {viewMode === 'PAPER' ? t.nav.paperMode : t.nav.liveMode} ·{' '}
              {isRunning
                ? t.nav.botRunning
                : isDraining
                  ? t.nav.botDraining
                  : t.nav.botStopped}
            </span>
          </div>
        </div>
      </aside>

      {/* Right Main Area (Top Header + Page Content) */}
      <div className="app-main-column">
        <header className="top-header">
          <div className="top-header-inner">
            {/* Left: Interactive Breadcrumb Navigation (Mobile Dropdown + Desktop Path) */}
            <div className="header-left" ref={breadcrumbRef}>
              <button
                type="button"
                className="breadcrumb-trigger"
                data-testid="mobile-breadcrumb-trigger"
                onClick={() => setMobileMenuOpen((prev) => !prev)}
                aria-expanded={mobileMenuOpen}
              >
                <span className="brand-dot" />
                <span className="breadcrumb-brand">Magnolia</span>
                <ChevronRight size={14} className="breadcrumb-sep" />
                <span className="breadcrumb-current">{activeNavItem.label}</span>
                <ChevronRight size={13} className="breadcrumb-sep breadcrumb-sub-sep" />
                <span className="breadcrumb-sub">{activeNavItem.subLabel}</span>
                <ChevronDown size={15} className="breadcrumb-caret" />
              </button>

              {mobileMenuOpen && (
                <div className="breadcrumb-dropdown" data-testid="mobile-breadcrumb-menu">
                  {navItems.map((item) => {
                    const Icon = item.icon;
                    return (
                      <button
                        key={item.key}
                        type="button"
                        className={`breadcrumb-dropdown-item ${item.active ? 'active' : ''}`}
                        onClick={() => {
                          navigate(item.to);
                          setMobileMenuOpen(false);
                        }}
                      >
                        <Icon size={16} />
                        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start' }}>
                          <span style={{ fontWeight: 600 }}>{item.label}</span>
                          <span style={{ fontSize: '11px', color: 'var(--text-muted)' }}>
                            {item.subLabel}
                          </span>
                        </div>
                      </button>
                    );
                  })}
                </div>
              )}
            </div>

            {/* Right: Mode Switch + Bot Start/Stop + Language + Logout */}
            <div className="header-right">
              <div className="mode-switch">
                <button
                  type="button"
                  className={`mode-switch-btn ${viewMode === 'PAPER' ? 'active-paper' : ''}`}
                  onClick={() => switchViewMode('PAPER')}
                >
                  <span
                    className={`status-dot ${
                      botStatuses.PAPER === 'RUNNING'
                        ? 'running'
                        : botStatuses.PAPER === 'DRAINING'
                          ? 'draining'
                          : 'stopped'
                    }`}
                  />
                  <span>{t.nav.paperMode}</span>
                </button>
                <button
                  type="button"
                  className={`mode-switch-btn ${viewMode === 'LIVE' ? 'active-live' : ''}`}
                  onClick={() => switchViewMode('LIVE')}
                >
                  <span
                    className={`status-dot ${
                      botStatuses.LIVE === 'RUNNING'
                        ? 'running'
                        : botStatuses.LIVE === 'DRAINING'
                          ? 'draining'
                          : 'stopped'
                    }`}
                  />
                  <span>{t.nav.liveMode}</span>
                </button>
              </div>

              <button
                type="button"
                disabled={updatingBot}
                onClick={handleBotButtonClick}
                className={`btn btn-sm ${isRunning ? 'btn-danger' : 'btn-emerald'}`}
              >
                {isRunning ? (
                  <>
                    <Square size={14} />
                    <span>{t.nav.stopBot}</span>
                  </>
                ) : (
                  <>
                    <Play size={14} />
                    <span>{isDraining ? t.nav.botDraining : t.nav.startBot}</span>
                  </>
                )}
              </button>

              <button
                type="button"
                onClick={toggleLanguage}
                className="btn btn-sm btn-ghost"
                title="Switch Language (KO / EN)"
              >
                <Globe size={14} />
                <span>{lang.toUpperCase()}</span>
              </button>

              <button
                type="button"
                onClick={logout}
                className="btn btn-sm btn-ghost"
                title={t.nav.logout}
              >
                <LogOut size={14} />
              </button>
            </div>
          </div>
        </header>

        <main className="page-container">
          <Outlet />
        </main>
      </div>

      {/* Stop Bot Modal when Active Positions Exist */}
      {showStopModal && (
        <div className="modal-backdrop">
          <div className="modal-sheet">
            <div>
              <h3 style={{ fontSize: '18px', fontWeight: 700, marginBottom: '6px' }}>
                {t.stopModal.title}
              </h3>
              <p style={{ fontSize: '14px', color: 'var(--text-secondary)' }}>
                {t.stopModal.desc}
              </p>
            </div>

            <div style={{ display: 'flex', flexDirection: 'column', gap: '12px' }}>
              <button
                type="button"
                disabled={updatingBot}
                onClick={() => executeBotCommand('DRAIN')}
                className="modal-option-btn"
              >
                <span style={{ fontSize: '15px', fontWeight: 700, color: '#60a5fa' }}>
                  {t.stopModal.optDrainTitle}
                </span>
                <span style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                  {t.stopModal.optDrainDesc}
                </span>
              </button>

              <button
                type="button"
                disabled={updatingBot}
                onClick={() => executeBotCommand('STOP_AND_CLOSE')}
                className="modal-option-btn"
                style={{ borderColor: 'rgba(244, 63, 94, 0.35)' }}
              >
                <span style={{ fontSize: '15px', fontWeight: 700, color: '#fb7185' }}>
                  {t.stopModal.optCloseTitle}
                </span>
                <span style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                  {t.stopModal.optCloseDesc}
                </span>
              </button>
            </div>

            <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
              <button
                type="button"
                className="btn btn-ghost"
                onClick={() => setShowStopModal(false)}
              >
                {t.stopModal.cancel}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};
