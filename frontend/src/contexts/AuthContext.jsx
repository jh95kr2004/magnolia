import React, { createContext, useContext, useState, useEffect } from 'react';
import { apiClient } from '../services/api';

const AuthContext = createContext(null);

export const AuthProvider = ({ children }) => {
  const [user, setUser] = useState(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    // Attempt to hydrate user from token on load
    const token = localStorage.getItem('magnolia_token');
    if (token) {
      apiClient.get('/auth/me')
        .then(res => setUser(res.data))
        .catch(() => {
          localStorage.removeItem('magnolia_token');
          setUser(null);
        })
        .finally(() => setLoading(false));
    } else {
      setLoading(false);
    }
  }, []);

  const login = async (email, password) => {
    const res = await apiClient.post('/auth/login', { email, password });
    const { token, user: userData } = res.data;
    localStorage.setItem('magnolia_token', token);
    setUser(userData);
  };

  const logout = () => {
    localStorage.removeItem('magnolia_token');
    setUser(null);
  };

  return (
    <AuthContext.Provider value={{ user, login, logout, isAuthenticated: !!user, loading }}>
      {children}
    </AuthContext.Provider>
  );
};

export const useAuth = () => useContext(AuthContext);
