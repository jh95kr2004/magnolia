import React from 'react';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { Layout } from './components/layout/Layout';
import { Auth } from './pages/Auth/Auth';
import { Dashboard } from './pages/Dashboard/Dashboard';
import { Opportunities } from './pages/Opportunities/Opportunities';
import { Settings } from './pages/Settings/Settings';
import { History } from './pages/History/History';
import { AuthProvider, useAuth } from './contexts/AuthContext';

// Protect private routes wrapper
const ProtectedRoute = ({ children }) => {
  const { isAuthenticated, loading } = useAuth();
  
  if (loading) return <div className="flex-center" style={{height:'100vh'}}>Loading...</div>;
  if (!isAuthenticated) return <Navigate to="/auth" replace />;
  
  return children;
};

export const App = () => {
  return (
    <AuthProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/auth" element={<Auth />} />
          
          <Route path="/" element={
            <ProtectedRoute>
              <Layout />
            </ProtectedRoute>
          }>
            <Route index element={<Navigate to="/dashboard" replace />} />
            <Route path="dashboard" element={<Dashboard />} />
            <Route path="opportunities" element={<Opportunities />} />
            <Route path="settings" element={<Settings />} />
            <Route path="history" element={<History />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </AuthProvider>
  );
};

export default App;
