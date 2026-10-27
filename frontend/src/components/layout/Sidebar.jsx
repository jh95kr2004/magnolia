import React from 'react';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import { LayoutDashboard, Settings, Activity, History, LogOut, Shield } from 'lucide-react';
import { useAuth } from '../../contexts/AuthContext';
import './Sidebar.css';

const navItems = [
  { path: '/dashboard', label: 'Control Center', icon: <LayoutDashboard size={20} /> },
  { path: '/opportunities', label: 'Global Scanner', icon: <Activity size={20} /> },
  { path: '/history', label: 'Trade History', icon: <History size={20} /> },
  { path: '/settings', label: 'Configuration', icon: <Settings size={20} /> },
];

export const Sidebar = () => {
  const location = useLocation();
  const navigate = useNavigate();
  const { user, logout } = useAuth();

  const handleLogout = () => {
    logout();
    navigate('/auth', { replace: true });
  };

  return (
    <aside className="sidebar glass-panel animate-slide-up">
      <div className="sidebar-header">
        <div className="logo-container animate-pulse">
          <div className="logo-orb"></div>
        </div>
        <div>
          <h1 className="logo-text text-gradient">Magnolia</h1>
          <div className="sidebar-subtitle">Delta-Neutral AI</div>
        </div>
      </div>

      <nav className="sidebar-nav">
        {navItems.map((item, idx) => (
          <NavLink
            key={item.path}
            to={item.path}
            className={`nav-link ${location.pathname.startsWith(item.path) ? 'active' : ''} animate-fade-in delay-${(idx + 1) * 100}`}
          >
            <span className="nav-icon">{item.icon}</span>
            <span className="nav-label">{item.label}</span>
          </NavLink>
        ))}
      </nav>

      <div className="sidebar-footer">
        {user && (
          <div className="sidebar-user-card">
            <div className="sidebar-user-info">
              <div className="sidebar-user-email" title={user.email}>
                {user.email}
              </div>
              <div className="sidebar-user-meta">
                <Shield size={11} />
                <span>Region: {user.region || 'KR'}</span>
              </div>
            </div>
          </div>
        )}
        <button className="logout-btn animate-fade-in delay-500" onClick={handleLogout}>
          <LogOut size={18} />
          <span>Sign Out</span>
        </button>
      </div>
    </aside>
  );
};
