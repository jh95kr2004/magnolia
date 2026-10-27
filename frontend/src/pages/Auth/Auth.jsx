import React, { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { Globe, Lock, Mail, UserPlus, LogIn } from 'lucide-react';
import { useAuth } from '../../contexts/AuthContext';
import './Auth.css';

export const Auth = () => {
  const [mode, setMode] = useState('login'); // 'login' | 'register'
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [region, setRegion] = useState('KR');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const { login, register, isAuthenticated } = useAuth();
  const navigate = useNavigate();

  useEffect(() => {
    if (isAuthenticated) {
      navigate('/dashboard', { replace: true });
    }
  }, [isAuthenticated, navigate]);

  const handleSubmit = async (e) => {
    e.preventDefault();
    setError('');

    if (mode === 'register' && password !== confirmPassword) {
      setError('Passwords do not match.');
      return;
    }

    try {
      setLoading(true);
      if (mode === 'login') {
        await login(email, password);
      } else {
        await register(email, password, region);
      }
    } catch (err) {
      const msg = err.response?.data || err.message || 'Authentication failed.';
      setError(typeof msg === 'string' ? msg : 'Invalid credentials or server offline.');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="auth-page flex-center" style={{ minHeight: '100vh', padding: '16px' }}>
      <div className="auth-login glass-panel animate-slide-up" style={{ width: '100%', maxWidth: '430px' }}>
        <div className="text-center mb-lg">
          <div className="logo-container mx-auto mb-md animate-pulse">
            <div className="logo-orb"></div>
          </div>
          <h1 className="text-2xl font-bold text-gradient">Magnolia</h1>
          <p className="text-secondary mt-xs text-sm">Global Delta-Neutral Arbitrage Engine</p>
        </div>

        {/* Mode Switcher Tabs */}
        <div className="auth-mode-tabs mb-lg">
          <button
            type="button"
            className={`auth-mode-tab ${mode === 'login' ? 'active' : ''}`}
            onClick={() => { setMode('login'); setError(''); }}
          >
            <LogIn size={15} /> Sign In
          </button>
          <button
            type="button"
            className={`auth-mode-tab ${mode === 'register' ? 'active' : ''}`}
            onClick={() => { setMode('register'); setError(''); }}
          >
            <UserPlus size={15} /> Create Account
          </button>
        </div>

        <form className="flex flex-col gap-md" onSubmit={handleSubmit}>
          {error && (
            <div className="auth-error-banner">
              {error}
            </div>
          )}

          <div className="form-group">
            <label className="text-xs font-semibold text-secondary uppercase tracking-wider flex items-center gap-xs">
              <Mail size={13} /> Email Address
            </label>
            <input
              type="email"
              placeholder="trader@magnolia.io"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="auth-input w-full mt-xs"
              required
            />
          </div>

          <div className="form-group">
            <label className="text-xs font-semibold text-secondary uppercase tracking-wider flex items-center gap-xs">
              <Lock size={13} /> Password
            </label>
            <input
              type="password"
              placeholder="••••••••"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="auth-input w-full mt-xs"
              required
            />
          </div>

          {mode === 'register' && (
            <>
              <div className="form-group">
                <label className="text-xs font-semibold text-secondary uppercase tracking-wider flex items-center gap-xs">
                  <Lock size={13} /> Confirm Password
                </label>
                <input
                  type="password"
                  placeholder="••••••••"
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  className="auth-input w-full mt-xs"
                  required
                />
              </div>

              <div className="form-group">
                <label className="text-xs font-semibold text-secondary uppercase tracking-wider flex items-center gap-xs">
                  <Globe size={13} /> Primary Jurisdiction / Base Currency Region
                </label>
                <select
                  value={region}
                  onChange={(e) => setRegion(e.target.value)}
                  className="auth-input w-full mt-xs"
                >
                  <option value="KR">KR — South Korea (KRW / Upbit / Bithumb)</option>
                  <option value="US">US — United States (USD / Coinbase / Kraken)</option>
                  <option value="GLOBAL">GLOBAL — Global Multi-Exchange (USDT / USD)</option>
                </select>
              </div>
            </>
          )}

          <button
            type="submit"
            disabled={loading}
            className="auth-submit-btn w-full mt-sm"
          >
            {loading
              ? 'Authenticating...'
              : mode === 'login'
                ? 'Sign In to Control Center'
                : 'Initialize Arbitrage Account'}
          </button>
        </form>
      </div>
    </div>
  );
};
