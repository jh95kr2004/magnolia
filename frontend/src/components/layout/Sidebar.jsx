import React from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import { LayoutDashboard, Settings, Activity, History, LogOut } from 'lucide-react';
import './Sidebar.css';

const navItems = [
  { path: '/dashboard', label: 'Dashboard', icon: <LayoutDashboard size={20} /> },
  { path: '/opportunities', label: 'Opportunities', icon: <Activity size={20} /> },
  { path: '/history', label: 'History', icon: <History size={20} /> },
  { path: '/settings', label: 'Settings', icon: <Settings size={20} /> },
];

export const Sidebar = () => {
  const location = useLocation();

  return (
    <aside className="sidebar glass-panel animate-slide-up">
      <div className="sidebar-header">
        <div className="logo-container animate-pulse">
          <div className="logo-orb"></div>
        </div>
        <h1 className="logo-text text-gradient">Magnolia</h1>
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
        <button className="logout-btn animate-fade-in delay-500">
          <LogOut size={20} />
          <span>Sign Out</span>
        </button>
      </div>
    </aside>
  );
};
