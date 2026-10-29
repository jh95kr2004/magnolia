import React from 'react';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { Layout } from './components/layout/Layout';
import { Auth } from './pages/Auth/Auth';
import { Dashboard } from './pages/Dashboard/Dashboard';
import { Opportunities } from './pages/Opportunities/Opportunities';
import { Settings } from './pages/Settings/Settings';
import { AuthProvider, useAuth } from './contexts/AuthContext';
import { LanguageProvider } from './contexts/LanguageContext';
import { CurrencyProvider } from './contexts/CurrencyContext';
import { ToastProvider } from './contexts/ToastContext';

const ProtectedRoute = ({ children }) => {
  const { isAuthenticated, loading } = useAuth();
  if (loading) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#94a3b8' }}>
        Loading...
      </div>
    );
  }
  if (!isAuthenticated) return <Navigate to="/auth" replace />;
  return children;
};

export const App = () => {
  return (
    <LanguageProvider>
      <CurrencyProvider>
        <ToastProvider>
          <AuthProvider>
            <BrowserRouter>
              <Routes>
                <Route path="/auth" element={<Auth />} />
                <Route
                  path="/"
                  element={
                    <ProtectedRoute>
                      <Layout />
                    </ProtectedRoute>
                  }
                >
                  <Route index element={<Navigate to="/dashboard" replace />} />
                  <Route path="dashboard" element={<Dashboard />} />
                  <Route path="opportunities" element={<Opportunities />} />
                  <Route path="settings" element={<Settings />} />
                  <Route path="history" element={<Navigate to="/settings?tab=history" replace />} />
                </Route>
              </Routes>
            </BrowserRouter>
          </AuthProvider>
        </ToastProvider>
      </CurrencyProvider>
    </LanguageProvider>
  );
};

export default App;
