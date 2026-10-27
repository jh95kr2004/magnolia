import React from 'react';
import { Outlet } from 'react-router-dom';
import { Sidebar } from './Sidebar';
import './Layout.css';

export const Layout = () => {
  return (
    <div className="app-layout">
      <Sidebar />
      <main className="main-content">
        <header className="topbar glass-panel animate-fade-in delay-200">
          <div className="search-bar">
            {/* Minimalist Topbar placeholder */}
          </div>
          <div className="user-profile">
            <div className="avatar">A</div>
          </div>
        </header>
        
        <div className="page-container animate-fade-in delay-300">
          <Outlet />
        </div>
      </main>
    </div>
  );
};
