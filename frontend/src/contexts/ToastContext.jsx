import React, { createContext, useContext, useState, useCallback } from 'react';
import { CheckCircle, AlertTriangle, XCircle, Info, X } from 'lucide-react';

const ToastContext = createContext(null);

export const ToastProvider = ({ children }) => {
  const [toasts, setToasts] = useState([]);
  const [confirmState, setConfirmState] = useState(null);

  const showToast = useCallback((message, type = 'info', duration = 4500) => {
    const id = Date.now() + Math.random();
    setToasts(prev => [...prev, { id, message, type }]);
    if (duration > 0) {
      setTimeout(() => {
        setToasts(prev => prev.filter(t => t.id !== id));
      }, duration);
    }
  }, []);

  const dismissToast = useCallback((id) => {
    setToasts(prev => prev.filter(t => t.id !== id));
  }, []);

  const requestConfirm = useCallback(({
    title = 'Confirm Action',
    message = 'Are you sure you want to proceed?',
    confirmText = 'Confirm',
    cancelText = 'Cancel',
    danger = false,
    onConfirm
  }) => {
    setConfirmState({
      title,
      message,
      confirmText,
      cancelText,
      danger,
      onConfirm
    });
  }, []);

  const handleConfirm = async () => {
    if (!confirmState) return;
    const cb = confirmState.onConfirm;
    setConfirmState(null);
    if (cb) {
      await cb();
    }
  };

  const getIcon = (type) => {
    switch (type) {
      case 'success': return <CheckCircle size={18} style={{ color: '#10b981', flexShrink: 0 }} />;
      case 'warning': return <AlertTriangle size={18} style={{ color: '#f59e0b', flexShrink: 0 }} />;
      case 'error':
      case 'critical': return <XCircle size={18} style={{ color: '#ef4444', flexShrink: 0 }} />;
      default: return <Info size={18} style={{ color: '#6366f1', flexShrink: 0 }} />;
    }
  };

  const getBorderColor = (type) => {
    switch (type) {
      case 'success': return 'rgba(16, 185, 129, 0.45)';
      case 'warning': return 'rgba(245, 158, 11, 0.45)';
      case 'error':
      case 'critical': return 'rgba(239, 68, 68, 0.55)';
      default: return 'rgba(99, 102, 241, 0.45)';
    }
  };

  return (
    <ToastContext.Provider value={{ showToast, requestConfirm }}>
      {children}

      {/* Global Toast Container */}
      <div
        style={{
          position: 'fixed',
          top: '20px',
          right: '20px',
          zIndex: 9999,
          display: 'flex',
          flexDirection: 'column',
          gap: '10px',
          maxWidth: '400px',
          width: '100%',
          pointerEvents: 'none'
        }}
      >
        {toasts.map(t => (
          <div
            key={t.id}
            className="animate-fade-in"
            style={{
              pointerEvents: 'auto',
              display: 'flex',
              alignItems: 'flex-start',
              justifyContent: 'space-between',
              gap: '12px',
              padding: '14px 16px',
              borderRadius: '12px',
              background: 'rgba(17, 19, 31, 0.95)',
              backdropFilter: 'blur(12px)',
              border: `1px solid ${getBorderColor(t.type)}`,
              boxShadow: '0 10px 30px rgba(0, 0, 0, 0.45)',
              color: '#f8fafc',
              fontSize: '0.875rem',
              lineHeight: '1.4'
            }}
          >
            <div style={{ display: 'flex', alignItems: 'flex-start', gap: '10px' }}>
              {getIcon(t.type)}
              <div style={{ wordBreak: 'break-word' }}>{t.message}</div>
            </div>
            <button
              onClick={() => dismissToast(t.id)}
              style={{
                color: '#94a3b8',
                background: 'transparent',
                border: 'none',
                padding: '2px',
                cursor: 'pointer'
              }}
            >
              <X size={15} />
            </button>
          </div>
        ))}
      </div>

      {/* Global Confirmation Modal */}
      {confirmState && (
        <div
          style={{
            position: 'fixed',
            inset: 0,
            background: 'rgba(0, 0, 0, 0.72)',
            backdropFilter: 'blur(4px)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 9998,
            padding: '16px'
          }}
        >
          <div
            className="glass-panel animate-fade-in"
            style={{
              maxWidth: '440px',
              width: '100%',
              padding: '24px',
              borderRadius: '16px',
              background: 'rgba(20, 22, 36, 0.98)',
              border: '1px solid rgba(255, 255, 255, 0.12)',
              boxShadow: '0 20px 50px rgba(0,0,0,0.6)'
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: '10px', marginBottom: '12px' }}>
              <AlertTriangle size={22} style={{ color: confirmState.danger ? '#ef4444' : '#fbbf24' }} />
              <h3 style={{ margin: 0, fontSize: '1.125rem', fontWeight: 700 }}>{confirmState.title}</h3>
            </div>
            <p style={{ color: '#cbd5e1', fontSize: '0.9rem', lineHeight: '1.5', whiteSpace: 'pre-line', marginBottom: '20px' }}>
              {confirmState.message}
            </p>
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '10px' }}>
              <button
                type="button"
                onClick={() => setConfirmState(null)}
                style={{
                  padding: '8px 16px',
                  borderRadius: '8px',
                  background: 'rgba(255, 255, 255, 0.06)',
                  border: '1px solid rgba(255, 255, 255, 0.12)',
                  color: '#cbd5e1',
                  fontWeight: 600,
                  fontSize: '0.875rem',
                  cursor: 'pointer'
                }}
              >
                {confirmState.cancelText}
              </button>
              <button
                type="button"
                onClick={handleConfirm}
                style={{
                  padding: '8px 18px',
                  borderRadius: '8px',
                  background: confirmState.danger ? '#ef4444' : 'var(--accent-primary)',
                  border: 'none',
                  color: '#fff',
                  fontWeight: 700,
                  fontSize: '0.875rem',
                  cursor: 'pointer'
                }}
              >
                {confirmState.confirmText}
              </button>
            </div>
          </div>
        </div>
      )}
    </ToastContext.Provider>
  );
};

export const useToast = () => useContext(ToastContext);
