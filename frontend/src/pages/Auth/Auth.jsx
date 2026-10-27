import React, { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useAuth } from '../../contexts/AuthContext';
import './Auth.css';

export const Auth = () => {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const { login, isAuthenticated } = useAuth();
  const navigate = useNavigate();

  useEffect(() => {
    if (isAuthenticated) {
      navigate('/dashboard', { replace: true });
    }
  }, [isAuthenticated, navigate]);

  const handleLogin = async (e) => {
    e.preventDefault();
    try {
      setError('');
      setLoading(true);
      await login(email, password);
    } catch (err) {
      setError('Invalid credentials or server offline.');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="auth-page flex-center" style={{ minHeight: '100vh' }}>
      <div className="auth-login glass-panel animate-slide-up" style={{ width: '100%', maxWidth: '400px' }}>
        <div className="text-center mb-xl">
          <div className="logo-container mx-auto mb-md animate-pulse">
            <div className="logo-orb"></div>
          </div>
          <h1 className="text-2xl font-bold text-gradient">Magnolia</h1>
          <p className="text-secondary mt-sm">Arbitrage Strategy Engine</p>
        </div>

        <form className="flex flex-col gap-md" onSubmit={handleLogin}>
          {error && <div className="text-sm font-semibold" style={{ color: 'var(--danger)', padding: 'var(--space-sm)', background: 'var(--danger-bg)', borderRadius: '8px' }}>{error}</div>}

          <div className="form-group">
            <label className="text-sm font-semibold text-secondary">Email Address</label>
            <input
              type="email"
              placeholder="admin@magnolia.test"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="w-full mt-xs"
              style={{ background: 'rgba(0,0,0,0.2)', border: '1px solid var(--border-light)', padding: 'var(--space-sm)', borderRadius: '8px', color: 'white' }}
              required
            />
          </div>

          <div className="form-group">
            <label className="text-sm font-semibold text-secondary">Password</label>
            <input
              type="password"
              placeholder="••••••••"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="w-full mt-xs"
              style={{ background: 'rgba(0,0,0,0.2)', border: '1px solid var(--border-light)', padding: 'var(--space-sm)', borderRadius: '8px', color: 'white' }}
              required
            />
          </div>

          <button
            type="submit"
            disabled={loading}
            className="w-full mt-md"
            style={{
              background: loading ? 'var(--text-muted)' : 'var(--accent-primary)',
              color: 'white',
              padding: 'var(--space-sm)',
              borderRadius: '8px',
              fontWeight: 'bold',
              cursor: loading ? 'not-allowed' : 'pointer'
            }}
          >
            {loading ? 'Authenticating...' : 'Sign In to Dashboard'}
          </button>
        </form>
      </div>
    </div>
  );
};
