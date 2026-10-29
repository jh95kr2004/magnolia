import React, { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Globe } from 'lucide-react';
import { useAuth } from '../../contexts/AuthContext';
import { useI18n } from '../../contexts/LanguageContext';
import { useToast } from '../../contexts/ToastContext';

export const Auth = () => {
  const [isLogin, setIsLogin] = useState(true);
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const { login, register } = useAuth();
  const { lang, toggleLanguage } = useI18n();
  const { showToast } = useToast();
  const navigate = useNavigate();

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!email || !password) return;
    setSubmitting(true);
    try {
      if (isLogin) {
        await login(email, password);
      } else {
        await register(email, password, 'kr');
      }
      navigate('/dashboard');
    } catch (err) {
      showToast(
        err.response?.data?.error ||
          (lang === 'ko' ? '로그인/회원가입에 실패했습니다.' : 'Authentication failed.'),
        'error'
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        justifyContent: 'center',
        padding: '24px',
        background: 'var(--bg-base)'
      }}
    >
      <div style={{ position: 'fixed', top: '24px', right: '28px' }}>
        <button type="button" onClick={toggleLanguage} className="btn btn-sm btn-ghost">
          <Globe size={14} />
          <span>{lang.toUpperCase()}</span>
        </button>
      </div>

      <div
        style={{
          width: '100%',
          maxWidth: '420px',
          background: 'var(--bg-surface)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 'var(--radius-lg)',
          padding: '40px 36px',
          display: 'flex',
          flexDirection: 'column',
          gap: '28px'
        }}
      >
        <div>
          <div className="brand-logo" style={{ fontSize: '22px', marginBottom: '10px' }}>
            <span className="brand-dot" />
            <span>Magnolia</span>
          </div>
          <h1 style={{ fontSize: '22px', fontWeight: 700, marginBottom: '6px' }}>
            {isLogin
              ? lang === 'ko'
                ? '계정에 로그인'
                : 'Sign in to Magnolia'
              : lang === 'ko'
                ? '새 계정 만들기'
                : 'Create your account'}
          </h1>
          <p style={{ fontSize: '14px', color: 'var(--text-secondary)' }}>
            {lang === 'ko'
              ? '글로벌 거래소 간 델타 중립 아비트라지 매매 플랫폼'
              : 'Global cross-exchange delta-neutral arbitrage platform'}
          </p>
        </div>

        <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: '18px' }}>
          <div className="field-group">
            <label className="field-label">{lang === 'ko' ? '이메일' : 'Email'}</label>
            <input
              type="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="name@example.com"
              className="input"
            />
          </div>

          <div className="field-group">
            <label className="field-label">{lang === 'ko' ? '비밀번호' : 'Password'}</label>
            <input
              type="password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="••••••••"
              className="input"
            />
          </div>

          <button
            type="submit"
            disabled={submitting}
            className="btn btn-primary"
            style={{ height: '44px', marginTop: '6px', width: '100%' }}
          >
            {submitting
              ? lang === 'ko'
                ? '처리 중...'
                : 'Processing...'
              : isLogin
                ? lang === 'ko'
                  ? '로그인'
                  : 'Sign In'
                : lang === 'ko'
                  ? '회원가입 및 시작하기'
                  : 'Create Account'}
          </button>
        </form>

        <div className="divider" />

        <div style={{ textAlign: 'center', fontSize: '14px', color: 'var(--text-secondary)' }}>
          {isLogin
            ? lang === 'ko'
              ? '아직 계정이 없으신가요? '
              : "Don't have an account? "
            : lang === 'ko'
              ? '이미 계정이 있으신가요? '
              : 'Already have an account? '}
          <button
            type="button"
            onClick={() => setIsLogin(!isLogin)}
            style={{ color: '#60a5fa', fontWeight: 600, marginLeft: '4px' }}
          >
            {isLogin
              ? lang === 'ko'
                ? '회원가입'
                : 'Sign Up'
              : lang === 'ko'
                ? '로그인'
                : 'Sign In'}
          </button>
        </div>
      </div>
    </div>
  );
};
