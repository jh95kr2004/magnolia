import React, { useState, useEffect } from 'react';
import { useSearchParams } from 'react-router-dom';
import { ExternalLink, RotateCcw, Download, X, Trash2 } from 'lucide-react';
import { apiClient } from '../../services/api';
import { useAuth } from '../../contexts/AuthContext';
import { useI18n } from '../../contexts/LanguageContext';
import { formatStrategyLabel } from '../../i18n/translations';
import { useToast } from '../../contexts/ToastContext';

export const Settings = () => {
  const [searchParams, setSearchParams] = useSearchParams();
  const { viewMode } = useAuth();
  const { lang, t } = useI18n();
  const { showToast, requestConfirm } = useToast();

  const initialTab = searchParams.get('tab') || 'exchange';
  const [activeTab, setActiveTab] = useState(initialTab);
  const [saving, setSaving] = useState(false);

  // 1. Exchange Keys & noVNC
  const [registeredKeys, setRegisteredKeys] = useState([]);
  const [supportedExchanges, setSupportedExchanges] = useState(['UPBIT', 'COINBASE']);
  const [apiKeys, setApiKeys] = useState({ exchange: 'UPBIT', access_key: '', secret_key: '' });
  const [showNoVNCModal, setShowNoVNCModal] = useState(searchParams.get('openNoVNC') === '1');
  const [noVNCUrl, setNoVNCUrl] = useState(() => {
    if (
      typeof window !== 'undefined' &&
      window.location.hostname !== 'localhost' &&
      window.location.hostname !== '127.0.0.1'
    ) {
      return `${window.location.origin}/novnc/`;
    }
    return 'http://localhost:6080';
  });

  // 2. Trading Parameters
  const [params, setParams] = useState({
    minTargetProfitPct: 1.5,
    maxStopLossPct: -5.0,
    capitalLimitUsd: 10000,
    maxAcceptableSpread: 0.01,
    rebalanceTriggerSkewPct: 20.0,
    rebalanceTargetSkewPct: 50.0,
    rebalanceMaxPremiumPct: 0.0
  });

  // 3. Paper Seed
  const [paperSeed, setPaperSeed] = useState({
    paperUpbitKrw: 50000000,
    paperCoinbaseUsd: 35000
  });

  // 4. Trade History
  const [historyList, setHistoryList] = useState([]);

  const switchSubTab = (tabName) => {
    setActiveTab(tabName);
    setSearchParams({ tab: tabName });
  };

  const loadSettingsData = async () => {
    try {
      const [keysRes, paramsRes, profileRes, exRes] = await Promise.all([
        apiClient.get('/settings/exchange').catch(() => ({ data: [] })),
        apiClient.get('/settings/parameters').catch(() => ({ data: null })),
        apiClient.get('/settings/profile').catch(() => ({ data: null })),
        apiClient.get('/settings/supported_exchanges').catch(() => ({ data: ['UPBIT', 'COINBASE'] }))
      ]);

      if (Array.isArray(keysRes.data)) setRegisteredKeys(keysRes.data);
      if (Array.isArray(exRes.data) && exRes.data.length > 0) {
        setSupportedExchanges(exRes.data);
      }
      if (paramsRes.data) {
        const d = paramsRes.data;
        setParams({
          minTargetProfitPct: d.MinTargetProfit ?? 1.5,
          maxStopLossPct: d.MaxStopLoss ?? -5.0,
          capitalLimitUsd: d.CapitalLimitUsd ?? 10000,
          maxAcceptableSpread: d.MaxAcceptableSpread ?? 0.01,
          rebalanceTriggerSkewPct: Number(d.RebalanceTriggerSkew ?? 0.2) * 100,
          rebalanceTargetSkewPct: Number(d.RebalanceTargetSkew ?? 0.5) * 100,
          rebalanceMaxPremiumPct: Number(d.RebalanceMaxPremium ?? 0.0) * 100
        });
      }
      if (profileRes.data) {
        setPaperSeed({
          paperUpbitKrw: profileRes.data.paper_upbit_krw || 50000000,
          paperCoinbaseUsd: profileRes.data.paper_coinbase_usd || 35000
        });
      }
    } catch {}
  };

  const loadHistoryData = async () => {
    try {
      const res = await apiClient.get(`/positions/history?mode=${viewMode}`);
      if (Array.isArray(res.data)) {
        setHistoryList(
          res.data.map((item) => ({
            id: item.PositionID || item.ID,
            baseExchange: item.BaseExchange || 'UPBIT',
            foreignExchange: item.ForeignExchange || 'COINBASE',
            baseSymbol: item.BaseSymbol || 'BTC-KRW',
            strategy: item.Type,
            investedUsd: Number(item.InvestmentUSD || 0),
            entrySpreadPct: Number((item.EntrySpread ?? item.EntryPremium) || 0) * 100,
            realizedPnlUsd: Number(item.RealizedProfitUSD ?? item.CurrentPnLUSD ?? 0),
            openedAt: item.OpenedAt,
            closedAt: item.ClosedAt || item.UpdatedAt
          }))
        );
      }
    } catch {}
  };

  useEffect(() => {
    loadSettingsData();
  }, []);

  useEffect(() => {
    loadHistoryData();
  }, [viewMode, activeTab]);

  const handleSaveExchangeKey = async (e) => {
    e.preventDefault();
    if (!apiKeys.exchange || !apiKeys.access_key || !apiKeys.secret_key) return;
    setSaving(true);
    try {
      await apiClient.post('/settings/exchange', apiKeys);
      setApiKeys({ ...apiKeys, access_key: '', secret_key: '' });
      await loadSettingsData();
      showToast(
        lang === 'ko'
          ? `${apiKeys.exchange} API 키가 안전하게 암호화되어 저장되었습니다.`
          : `${apiKeys.exchange} API key saved.`,
        'success'
      );
    } catch (err) {
      showToast(err.response?.data?.error || err.message, 'error');
    } finally {
      setSaving(false);
    }
  };

  const handleToggleKey = async (exchangeName, currentActive) => {
    try {
      await apiClient.post('/settings/exchange/toggle', {
        exchange: exchangeName,
        is_active: !currentActive,
        force: true
      });
      await loadSettingsData();
      showToast(
        lang === 'ko'
          ? `${exchangeName} 연결 상태가 변경되었습니다.`
          : `${exchangeName} status updated.`,
        'success'
      );
    } catch (err) {
      showToast(err.response?.data?.error || err.message, 'error');
    }
  };

  const handleDeleteKey = (exchangeName) => {
    requestConfirm({
      title: lang === 'ko' ? `${exchangeName} API 키 삭제` : `Delete ${exchangeName} Key`,
      message:
        lang === 'ko'
          ? `등록된 ${exchangeName} API 키를 삭제하시겠습니까?`
          : `Remove ${exchangeName} credentials?`,
      confirmText: lang === 'ko' ? '삭제' : 'Delete',
      cancelText: t.stopModal.cancel,
      danger: true,
      onConfirm: async () => {
        try {
          await apiClient.post('/settings/exchange/delete', {
            exchange: exchangeName,
            force: true
          });
          await loadSettingsData();
          showToast(
            lang === 'ko' ? `${exchangeName} API 키가 삭제되었습니다.` : 'API key deleted.',
            'success'
          );
        } catch (err) {
          showToast(err.response?.data?.error || err.message, 'error');
        }
      }
    });
  };

  const handleSaveParams = async (e) => {
    e.preventDefault();
    setSaving(true);
    try {
      await apiClient.post('/settings/parameters', {
        strategy_name: 'PremiumStrategy',
        min_target_profit: Number(params.minTargetProfitPct),
        max_stop_loss: Number(params.maxStopLossPct),
        max_acceptable_spread: Number(params.maxAcceptableSpread),
        capital_limit_usd: Number(params.capitalLimitUsd),
        max_risk_score: 80.0,
        risk_multiplier: 1.0,
        rebalance_trigger_skew: Number(params.rebalanceTriggerSkewPct) / 100.0,
        rebalance_target_skew: Number(params.rebalanceTargetSkewPct) / 100.0,
        rebalance_max_premium: Number(params.rebalanceMaxPremiumPct) / 100.0
      });
      showToast(
        lang === 'ko' ? '자동매매 기준 설정이 저장되었습니다.' : 'Parameters saved.',
        'success'
      );
    } catch (err) {
      showToast(err.response?.data?.error || err.message, 'error');
    } finally {
      setSaving(false);
    }
  };

  const handleResetPaperAccount = () => {
    requestConfirm({
      title: lang === 'ko' ? '모의매매 계좌 초기화' : 'Reset Paper Account',
      message:
        lang === 'ko'
          ? `모의매매 계좌를 ₩${Number(paperSeed.paperUpbitKrw).toLocaleString()} KRW 및 $${Number(paperSeed.paperCoinbaseUsd).toLocaleString()} USD로 초기화하고 열린 가상 포지션을 모두 정리하시겠습니까?`
          : 'Reset paper balances and close all open virtual positions?',
      confirmText: t.settings.resetPaperBtn,
      cancelText: t.stopModal.cancel,
      danger: true,
      onConfirm: async () => {
        setSaving(true);
        try {
          await apiClient.post('/settings/paper_reset', {
            paper_upbit_krw: Number(paperSeed.paperUpbitKrw),
            paper_coinbase_usd: Number(paperSeed.paperCoinbaseUsd)
          });
          showToast(
            lang === 'ko'
              ? '모의매매 계좌가 설정된 시드로 초기화되었습니다.'
              : 'Paper account reset complete.',
            'success'
          );
        } catch (err) {
          showToast(err.response?.data?.error || err.message, 'error');
        } finally {
          setSaving(false);
        }
      }
    });
  };

  const exportHistoryCsv = () => {
    if (historyList.length === 0) return;
    const headers = ['ID', 'Pair', 'Strategy', 'InvestedUSD', 'EntrySpreadPct', 'RealizedPnLUSD', 'OpenedAt', 'ClosedAt'];
    const rows = historyList.map((h) => [
      h.id,
      `${h.baseSymbol} (${h.baseExchange}->${h.foreignExchange})`,
      h.strategy,
      h.investedUsd.toFixed(2),
      h.entrySpreadPct.toFixed(2),
      h.realizedPnlUsd.toFixed(2),
      h.openedAt || '',
      h.closedAt || ''
    ]);
    const csv = [headers.join(','), ...rows.map((r) => r.join(','))].join('\n');
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8;' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `magnolia_history_${viewMode.toLowerCase()}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <>
      <div className="page-header">
        <div>
          <h1 className="page-title">{t.settings.title}</h1>
          <p className="page-subtitle">{t.settings.subtitle}</p>
        </div>
      </div>

      {/* Sub-Navigation Bar */}
      <div className="sub-tabs">
        <button
          type="button"
          className={`sub-tab ${activeTab === 'exchange' ? 'active' : ''}`}
          onClick={() => switchSubTab('exchange')}
        >
          {t.settings.tabExchange}
        </button>
        <button
          type="button"
          className={`sub-tab ${activeTab === 'params' ? 'active' : ''}`}
          onClick={() => switchSubTab('params')}
        >
          {t.settings.tabParams}
        </button>
        <button
          type="button"
          className={`sub-tab ${activeTab === 'paper' ? 'active' : ''}`}
          onClick={() => switchSubTab('paper')}
        >
          {t.settings.tabPaper}
        </button>
        <button
          type="button"
          className={`sub-tab ${activeTab === 'history' ? 'active' : ''}`}
          onClick={() => switchSubTab('history')}
        >
          {t.settings.tabHistory}
        </button>
      </div>

      {/* Sub-Tab 1: Exchange API Keys & Web Lending */}
      {activeTab === 'exchange' && (
        <div className="section-block" style={{ gap: '32px' }}>
          <div className="section-header">
            <h2 className="section-title">{t.settings.exchangeListTitle}</h2>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              onClick={() => setShowNoVNCModal(true)}
            >
              <span>{t.settings.openNoVncBtn}</span>
              <ExternalLink size={14} />
            </button>
          </div>

          <div className="table-surface">
            {registeredKeys.length === 0 ? (
              <div className="empty-state" style={{ padding: '40px 24px' }}>
                <div>
                  {lang === 'ko'
                    ? '등록된 거래소 API 키가 없습니다. (모의매매는 API 키 없이도 100% 실제 시세로 즉시 작동합니다)'
                    : 'No exchange API keys registered yet. (Paper mode works immediately without API keys)'}
                </div>
              </div>
            ) : (
              <table className="clean-table">
                <thead>
                  <tr>
                    <th>{lang === 'ko' ? '거래소' : 'Exchange'}</th>
                    <th>{lang === 'ko' ? '상태' : 'Status'}</th>
                    <th>{lang === 'ko' ? '등록된 키 (마스킹)' : 'Masked Key'}</th>
                    <th style={{ textAlign: 'right' }}>{lang === 'ko' ? '관리' : 'Actions'}</th>
                  </tr>
                </thead>
                <tbody>
                  {registeredKeys.map((k) => {
                    const exName = k.exchange || k.Exchange;
                    const isActive = k.is_active ?? k.IsActive ?? true;
                    const access = k.access_key || k.AccessKey || '';
                    return (
                      <tr key={exName}>
                        <td className="mono" style={{ fontWeight: 700 }}>
                          {exName}
                        </td>
                        <td>
                          <span className={`badge ${isActive ? 'badge-emerald' : 'badge-neutral'}`}>
                            {isActive
                              ? lang === 'ko'
                                ? '활성화됨'
                                : 'Active'
                              : lang === 'ko'
                                ? '비활성화'
                                : 'Disabled'}
                          </span>
                        </td>
                        <td className="mono" style={{ color: 'var(--text-secondary)' }}>
                          {access ? `${access.slice(0, 6)}••••••` : '••••••••'}
                        </td>
                        <td style={{ textAlign: 'right' }}>
                          <div style={{ display: 'inline-flex', gap: '8px' }}>
                            <button
                              type="button"
                              className="btn btn-sm btn-ghost"
                              onClick={() => handleToggleKey(exName, isActive)}
                            >
                              {isActive
                                ? lang === 'ko'
                                  ? '일시 중지'
                                  : 'Disable'
                                : lang === 'ko'
                                  ? '활성화'
                                  : 'Enable'}
                            </button>
                            <button
                              type="button"
                              className="btn btn-sm btn-danger"
                              onClick={() => handleDeleteKey(exName)}
                            >
                              <Trash2 size={14} />
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>

          <form className="form-panel" onSubmit={handleSaveExchangeKey}>
            <h3 className="section-title">{t.settings.addExchangeTitle}</h3>
            <div className="form-grid-3">
              <div className="field-group">
                <label className="field-label">{t.settings.exchangeLabel}</label>
                <select
                  value={apiKeys.exchange}
                  onChange={(e) => setApiKeys({ ...apiKeys, exchange: e.target.value })}
                  className="select"
                >
                  {supportedExchanges.map((ex) => (
                    <option key={ex} value={ex}>
                      {ex}
                    </option>
                  ))}
                </select>
              </div>

              <div className="field-group">
                <label className="field-label">{t.settings.accessKeyLabel}</label>
                <input
                  type="text"
                  required
                  value={apiKeys.access_key}
                  onChange={(e) => setApiKeys({ ...apiKeys, access_key: e.target.value })}
                  placeholder="Access Key / API Key ID"
                  className="input mono"
                />
              </div>

              <div className="field-group">
                <label className="field-label">{t.settings.secretKeyLabel}</label>
                <input
                  type="password"
                  required
                  value={apiKeys.secret_key}
                  onChange={(e) => setApiKeys({ ...apiKeys, secret_key: e.target.value })}
                  placeholder="Secret Key"
                  className="input mono"
                />
              </div>
            </div>

            <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
              <button type="submit" disabled={saving} className="btn btn-primary">
                {t.settings.saveKeyBtn}
              </button>
            </div>
          </form>
        </div>
      )}

      {/* Sub-Tab 2: Trading Thresholds & Rebalance Parameters */}
      {activeTab === 'params' && (
        <form className="form-panel" onSubmit={handleSaveParams}>
          <div>
            <h2 className="section-title">{t.settings.paramsTitle}</h2>
            <p className="section-desc" style={{ marginTop: '4px' }}>
              {lang === 'ko'
                ? '포지션 익절·손절 기준과 해외 거래소 자본 쏠림 방지(100% 델타 중립 자동 역전송 리밸런싱) 기준을 설정합니다.'
                : 'Configure profit/loss exits and automatic 100% delta-neutral reverse transfer rebalancing.'}
            </p>
          </div>

          <div className="form-grid-2">
            <div className="field-group">
              <label className="field-label">{t.settings.minProfitLabel}</label>
              <input
                type="number"
                step="0.1"
                value={params.minTargetProfitPct}
                onChange={(e) => setParams({ ...params, minTargetProfitPct: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group">
              <label className="field-label">{t.settings.maxLossLabel}</label>
              <input
                type="number"
                step="0.1"
                value={params.maxStopLossPct}
                onChange={(e) => setParams({ ...params, maxStopLossPct: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group">
              <label className="field-label">{t.settings.capitalLimitLabel}</label>
              <input
                type="number"
                step="100"
                value={params.capitalLimitUsd}
                onChange={(e) => setParams({ ...params, capitalLimitUsd: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group">
              <label className="field-label">{t.settings.maxSpreadLabel}</label>
              <input
                type="number"
                step="0.001"
                value={params.maxAcceptableSpread}
                onChange={(e) => setParams({ ...params, maxAcceptableSpread: e.target.value })}
                className="input mono"
              />
            </div>
          </div>

          <div className="divider" />

          <div className="form-grid-3">
            <div className="field-group">
              <label className="field-label">{t.settings.rebalanceTriggerLabel}</label>
              <input
                type="number"
                step="1"
                min="5"
                max="45"
                value={params.rebalanceTriggerSkewPct}
                onChange={(e) => setParams({ ...params, balanceTrigger: e.target.value, rebalanceTriggerSkewPct: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group">
              <label className="field-label">{t.settings.rebalanceTargetLabel}</label>
              <input
                type="number"
                step="1"
                min="30"
                max="70"
                value={params.rebalanceTargetSkewPct}
                onChange={(e) => setParams({ ...params, rebalanceTargetSkewPct: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group">
              <label className="field-label">{t.settings.rebalanceMaxPremLabel}</label>
              <input
                type="number"
                step="0.05"
                value={params.rebalanceMaxPremiumPct}
                onChange={(e) => setParams({ ...params, rebalanceMaxPremiumPct: e.target.value })}
                className="input mono"
              />
            </div>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <button type="submit" disabled={saving} className="btn btn-primary">
              {t.settings.saveParamsBtn}
            </button>
          </div>
        </form>
      )}

      {/* Sub-Tab 3: Paper Trading Virtual Seed Management */}
      {activeTab === 'paper' && (
        <div className="form-panel">
          <div>
            <h2 className="section-title">{t.settings.paperTitle}</h2>
            <p className="section-desc" style={{ marginTop: '4px' }}>
              {t.settings.paperDesc}
            </p>
          </div>

          <div className="form-grid-2">
            <div className="field-group">
              <label className="field-label">{t.settings.paperUpbitKrwLabel}</label>
              <input
                type="number"
                step="1000000"
                value={paperSeed.paperUpbitKrw}
                onChange={(e) => setPaperSeed({ ...paperSeed, paperUpbitKrw: e.target.value })}
                className="input mono"
              />
            </div>

            <div className="field-group">
              <label className="field-label">{t.settings.paperCoinbaseUsdLabel}</label>
              <input
                type="number"
                step="1000"
                value={paperSeed.paperCoinbaseUsd}
                onChange={(e) => setPaperSeed({ ...paperSeed, paperCoinbaseUsd: e.target.value })}
                className="input mono"
              />
            </div>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <button
              type="button"
              disabled={saving}
              onClick={handleResetPaperAccount}
              className="btn btn-danger"
            >
              <RotateCcw size={15} />
              <span>{t.settings.resetPaperBtn}</span>
            </button>
          </div>
        </div>
      )}

      {/* Sub-Tab 4: Closed Trade History */}
      {activeTab === 'history' && (
        <div className="section-block">
          <div className="section-header">
            <h2 className="section-title">
              <span>{t.settings.historyTitle}</span>
              <span className="badge badge-neutral mono">{historyList.length}</span>
            </h2>
            {historyList.length > 0 && (
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={exportHistoryCsv}
              >
                <Download size={14} />
                <span>{t.settings.exportCsvBtn}</span>
              </button>
            )}
          </div>

          <div className="table-surface">
            {historyList.length === 0 ? (
              <div className="empty-state">
                <div>{t.settings.noHistory}</div>
              </div>
            ) : (
              <table className="clean-table">
                <thead>
                  <tr>
                    <th>{t.dashboard.colAsset}</th>
                    <th>{t.dashboard.colStrategy}</th>
                    <th>{t.dashboard.colInvested}</th>
                    <th>{t.dashboard.colEntrySpread}</th>
                    <th>{t.settings.colRealizedPnl}</th>
                    <th>{t.settings.colOpenedAt}</th>
                    <th>{t.settings.colClosedAt}</th>
                  </tr>
                </thead>
                <tbody>
                  {historyList.map((h) => {
                    const coin = String(h.baseSymbol).split('-')[0];
                    const isWin = h.realizedPnlUsd >= 0;
                    return (
                      <tr key={h.id}>
                        <td>
                          <div className="mono" style={{ fontWeight: 700 }}>
                            {coin}
                          </div>
                          <div style={{ fontSize: '12px', color: 'var(--text-secondary)' }}>
                            {h.baseExchange} → {h.foreignExchange}
                          </div>
                        </td>
                        <td>{formatStrategyLabel(h.strategy, lang)}</td>
                        <td className="mono">${h.investedUsd.toFixed(2)}</td>
                        <td className="mono">
                          {h.entrySpreadPct >= 0 ? '+' : ''}
                          {h.entrySpreadPct.toFixed(2)}%
                        </td>
                        <td
                          className="mono"
                          style={{
                            fontWeight: 700,
                            color: isWin ? 'var(--emerald)' : 'var(--rose)'
                          }}
                        >
                          {isWin ? '+' : ''}${h.realizedPnlUsd.toFixed(2)}
                        </td>
                        <td className="mono" style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                          {h.openedAt ? new Date(h.openedAt).toLocaleString() : '-'}
                        </td>
                        <td className="mono" style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                          {h.closedAt ? new Date(h.closedAt).toLocaleString() : '-'}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>
        </div>
      )}

      {/* noVNC Web Lending Browser Modal */}
      {showNoVNCModal && (
        <div className="modal-backdrop">
          <div
            className="modal-sheet"
            style={{ maxWidth: '1040px', height: '82vh', padding: '24px' }}
          >
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '16px' }}>
              <div>
                <h3 style={{ fontSize: '18px', fontWeight: 700 }}>
                  {t.settings.openNoVncBtn}
                </h3>
                <p style={{ fontSize: '13px', color: 'var(--text-secondary)' }}>
                  {lang === 'ko'
                    ? '아래 원격 브라우저에서 업비트 로그인을 완료하면 엔진이 자동으로 대여 세션을 감지·유지합니다.'
                    : 'Complete Upbit login below. The engine automatically monitors your lending session.'}
                </p>
              </div>
              <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                <input
                  type="text"
                  value={noVNCUrl}
                  onChange={(e) => setNoVNCUrl(e.target.value)}
                  className="input mono"
                  style={{ width: '240px', height: '36px', fontSize: '12px' }}
                />
                <button
                  type="button"
                  className="btn btn-sm btn-ghost"
                  onClick={() => setShowNoVNCModal(false)}
                >
                  <X size={16} />
                </button>
              </div>
            </div>

            <iframe
              src={noVNCUrl}
              title="noVNC Exchange Session"
              style={{
                width: '100%',
                flex: 1,
                borderRadius: 'var(--radius-md)',
                border: '1px solid var(--border-subtle)',
                background: '#000'
              }}
            />
          </div>
        </div>
      )}
    </>
  );
};
