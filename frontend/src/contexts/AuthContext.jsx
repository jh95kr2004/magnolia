import React, { createContext, useContext, useState, useEffect } from 'react';
import { apiClient } from '../services/api';

const AuthContext = createContext(null);

export const AuthProvider = ({ children }) => {
  const [user, setUser] = useState(null);
  const [loading, setLoading] = useState(true);
  const [viewMode, setViewMode] = useState(() => {
    const saved = localStorage.getItem('magnolia_view_mode');
    return saved === 'LIVE' ? 'LIVE' : 'PAPER';
  });
  const [botStatuses, setBotStatuses] = useState({
    PAPER: 'RUNNING',
    LIVE: 'STOPPED'
  });

  const refreshBotStatuses = async () => {
    try {
      const res = await apiClient.get('/settings/bot_status');
      if (res.data) {
        setBotStatuses({
          PAPER: res.data.paper_bot_status || 'RUNNING',
          LIVE: res.data.live_bot_status || 'STOPPED'
        });
      }
    } catch (err) {}
  };

  useEffect(() => {
    const token = localStorage.getItem('magnolia_token');
    if (token) {
      apiClient.get('/auth/me')
        .then(res => {
          setUser(res.data);
          refreshBotStatuses();
        })
        .catch(() => {
          localStorage.removeItem('magnolia_token');
          setUser(null);
        })
        .finally(() => setLoading(false));
    } else {
      setLoading(false);
    }
  }, []);

  const switchViewMode = async (nextMode) => {
    const normalized = nextMode === 'LIVE' ? 'LIVE' : 'PAPER';
    setViewMode(normalized);
    localStorage.setItem('magnolia_view_mode', normalized);
    try {
      await apiClient.post('/settings/bot_status', {
        active_view_mode: normalized,
        sub_action: 'VIEW_ONLY'
      });
    } catch (err) {}
  };

  const login = async (email, password) => {
    const res = await apiClient.post('/auth/login', { email, password });
    const { token, user: userData } = res.data;
    localStorage.setItem('magnolia_token', token);
    setUser(userData);
    refreshBotStatuses();
    return userData;
  };

  const register = async (email, password, region = 'KR') => {
    const res = await apiClient.post('/auth/register', { email, password, region });
    const { token, user: userData } = res.data;
    localStorage.setItem('magnolia_token', token);
    setUser(userData);
    refreshBotStatuses();
    return userData;
  };

  const logout = () => {
    localStorage.removeItem('magnolia_token');
    setUser(null);
  };

  return (
    <AuthContext.Provider value={{
      user,
      login,
      register,
      logout,
      isAuthenticated: !!user,
      loading,
      viewMode,
      switchViewMode,
      botStatuses,
      setBotStatuses,
      refreshBotStatuses
    }}>
      {children}
    </AuthContext.Provider>
  );
};

export const useAuth = () => useContext(AuthContext);

export const buildSseUrl = (path, modeOverride) => {
  const token = localStorage.getItem('magnolia_token') || '';
  const mode = modeOverride || localStorage.getItem('magnolia_view_mode') || 'PAPER';
  const baseHost = import.meta.env?.VITE_API_BASE_URL || '';
  const separator = path.includes('?') ? '&' : '?';
  return `${baseHost}${path}${separator}token=${encodeURIComponent(token)}&mode=${encodeURIComponent(mode)}`;
};
