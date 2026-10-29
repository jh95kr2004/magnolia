import React, { useState } from 'react';
import { Outlet, NavLink, useLocation, useNavigate } from 'react-router-dom';
import { Play, Square, Globe, LogOut } from 'lucide-react';
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

  const currentBotStatus = botStatuses[viewMode] || (viewMode === 'PAPER' ? 'RUNNING' : 'STOPPED');
  const isRunning = currentBotStatus === 'RUNNING';
  const isDraining = currentBotStatus === 'DRAINING';

  const executeBotCommand = async (subAction) => {
    setUpdatingBot(true);
    try {
      const res = await apiClient.post('/settings/bot_status', {
        mode: viewMode,
        sub_action: subAction
      });
      if (res.data) {
        setBotStatuses({
          PAPER: res.data.paper_bot_status || botStatuses.PAPER,
          LIVE: res.data.live_bot_status || botStatuses.LIVE
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
    // Check if there are open positions before stopping
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

  const isSettingsActive =
    location.pathname.startsWith('/settings') || location.pathname.startsWith('/history');

  return (
    <div className="app-shell">
      <header className="top-header">
        <div className="top-header-inner">
          {/* Left: Brand & 3 Clean Tabs */}
          <div className="header-left">
            <div
              className="brand-logo"
              style={{ cursor: 'pointer' }}
              onClick={() => navigate('/dashboard')}
            >
              <span className="brand-dot" />
              <span>Magnolia</span>
            </div>

            <nav className="nav-tabs">
              <NavLink
                to="/dashboard"
                className={({ isActive }) => `nav-tab ${isActive ? 'active' : ''}`}
              >
                {t.nav.dashboard}
              </NavLink>
              <NavLink
                to="/opportunities"
                className={({ isActive }) => `nav-tab ${isActive ? 'active' : ''}`}
              >
                {t.nav.market}
              </NavLink>
              <NavLink
                to="/settings"
                className={() => `nav-tab ${isSettingsActive ? 'active' : ''}`}
              >
                {t.nav.settings}
              </NavLink>
            </nav>
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
