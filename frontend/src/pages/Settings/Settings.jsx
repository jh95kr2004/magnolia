import React, { useState, useEffect } from 'react';
import { useSearchParams } from 'react-router-dom';
import { Save, Key, Sliders, Bell, AlertTriangle, AlertCircle, XCircle, Clock, X, Scale } from 'lucide-react';
import { Toggle } from '../../components/ui/Toggle';
import { apiClient } from '../../services/api';
import { useToast } from '../../contexts/ToastContext';
import './Settings.css';

export const Settings = () => {
  const [searchParams] = useSearchParams();
  const { showToast } = useToast();

  const [activeTab, setActiveTab] = useState(searchParams.get('tab') || 'exchange');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  // Data State
  const [registeredKeys, setRegisteredKeys] = useState([]);
  const [supportedExchanges, setSupportedExchanges] = useState([]);
  const [editMode, setEditMode] = useState(false);
  const [popupConfig, setPopupConfig] = useState(null);
  const [showNoVNCModal, setShowNoVNCModal] = useState(searchParams.get('openNoVNC') === '1');
  const [noVNCUrl, setNoVNCUrl] = useState('http://localhost:6080');
  const [apiKeys, setApiKeys] = useState({ exchange: '', access_key: '', secret_key: '' });
  const [params, setParams] = useState({
    usePercentParams: false,
    minTargetProfitUsd: 10,
    maxStopLossUsd: 50,
    minTargetProfitPct: 1.0,
    maxStopLossPct: -10.0,
    maxAcceptableSpread: 0.01,
    capitalLimitUsd: 10000,
    maxRiskScore: 50.0,
    riskMultiplier: 1.5,
    rebalanceTriggerSkewPct: 20.0,
    rebalanceTargetSkewPct: 50.0,
    rebalanceMaxPremiumPct: 0.0
  });
  const [profile, setProfile] = useState({
    notifyOnEntry: true,
    notifyOnExit: true,
    discordWebhook: '',
    telegramChatId: '',
    partialExitMode: 'FIXED',
    partialExitRatio: 0.5,
    enableStrategicSwitching: false,
    switchingProfitMultiplier: 2.0
  });

  // Load Data
  useEffect(() => {
    const fetchData = async () => {
      try {
        const [keysRes, paramsRes, profileRes, exchangesRes] = await Promise.all([
          apiClient.get('/settings/exchange'),
          apiClient.get('/settings/parameters'),
          apiClient.get('/settings/profile'),
          apiClient.get('/settings/supported_exchanges')
        ]);

        if (exchangesRes.data && exchangesRes.data.length > 0) {
          setSupportedExchanges(exchangesRes.data);
          setApiKeys(prev => ({ ...prev, exchange: prev.exchange || exchangesRes.data[0] }));
        }

        if (keysRes.data) {
          setRegisteredKeys(keysRes.data);
        }

        if (paramsRes.data && paramsRes.data.ID) {
          setParams({
            usePercentParams: paramsRes.data.UsePercentParams || false,
            minTargetProfitUsd: paramsRes.data.MinTargetProfit || 10,
            maxStopLossUsd: paramsRes.data.MaxStopLoss || 50,
            minTargetProfitPct: paramsRes.data.MinTargetProfitPercent || 1.0,
            maxStopLossPct: paramsRes.data.MaxStopLossPercent || -10.0,
            maxAcceptableSpread: paramsRes.data.MaxAcceptableSpread || 0.01,
            capitalLimitUsd: paramsRes.data.CapitalLimitUsd || 10000,
            maxRiskScore: paramsRes.data.MaxRiskScore || 50.0,
            riskMultiplier: paramsRes.data.RiskMultiplier || 1.5,
            rebalanceTriggerSkewPct: (paramsRes.data.RebalanceTriggerSkew || 0.20) * 100,
            rebalanceTargetSkewPct: (paramsRes.data.RebalanceTargetSkew || 0.50) * 100,
            rebalanceMaxPremiumPct: (paramsRes.data.RebalanceMaxPremium || 0.0) * 100
          });
        }

        if (profileRes.data && profileRes.data.ID) {
          setProfile({
            notifyOnEntry: profileRes.data.NotifyOnEntry,
            notifyOnExit: profileRes.data.NotifyOnExit,
            discordWebhook: profileRes.data.DiscordWebhookURL || '',
            telegramChatId: profileRes.data.TelegramChatID || '',
            partialExitMode: profileRes.data.PartialExitMode || 'FIXED',
            partialExitRatio: profileRes.data.PartialExitRatio || 0.5,
            enableStrategicSwitching: profileRes.data.EnableStrategicSwitching || false,
            switchingProfitMultiplier: profileRes.data.SwitchingProfitMultiplier || 2.0
          });
        }
      } catch (err) {
        showToast('Failed to load user configuration.', 'error');
      } finally {
        setLoading(false);
      }
    };
    fetchData();
  }, [showToast]);

  const handleSaveKeys = async (e) => {
    e.preventDefault();
    setSaving(true);
    try {
      await apiClient.post('/settings/exchange', apiKeys);
      setApiKeys(prev => ({ ...prev, access_key: '', secret_key: '' }));
      const keysRes = await apiClient.get('/settings/exchange');
      if (keysRes.data) setRegisteredKeys(keysRes.data);
      showToast(`${apiKeys.exchange} API keys verified and saved!`, 'success');
    } catch (err) {
      showToast('Verification failed: ' + (err.response?.data?.error || err.message), 'error');
    } finally {
      setSaving(false);
    }
  };

  const handleToggleExchange = async (exchange, newState) => {
    try {
      await apiClient.post('/settings/exchange/toggle', { exchange, is_active: newState, force: false });
      const keysRes = await apiClient.get('/settings/exchange');
      if (keysRes.data) setRegisteredKeys(keysRes.data);
      showToast(`${exchange} ${newState ? 'enabled' : 'paused'}.`, 'info');
    } catch (err) {
      if (err.response && err.response.status === 409) {
        setPopupConfig({ type: 'toggle', exchange, targetState: newState });
      } else {
        showToast('Toggle failed: ' + (err.response?.data?.error || err.message), 'error');
      }
    }
  };

  const handleDeleteExchange = async (exchange) => {
    try {
      await apiClient.post('/settings/exchange/delete', { exchange, force: false });
      const keysRes = await apiClient.get('/settings/exchange');
      if (keysRes.data) setRegisteredKeys(keysRes.data);
      showToast(`${exchange} connection removed.`, 'info');
    } catch (err) {
      if (err.response && err.response.status === 409) {
        setPopupConfig({ type: 'delete', exchange });
      } else {
        showToast('Delete failed: ' + (err.response?.data?.error || err.message), 'error');
      }
    }
  };

  const confirmAction = async (force, forceOption) => {
    if (!popupConfig) return;
    setSaving(true);
    try {
      if (popupConfig.type === 'delete') {
        if (forceOption === 'close_and_remove') {
          await apiClient.post('/settings/exchange/delete', { exchange: popupConfig.exchange, force: true });
        }
      } else if (popupConfig.type === 'toggle') {
        if (forceOption === 'close_and_disable' || forceOption === 'keep_and_disable') {
          await apiClient.post('/settings/exchange/toggle', { exchange: popupConfig.exchange, is_active: popupConfig.targetState, force: true });
        }
      }
      const keysRes = await apiClient.get('/settings/exchange');
      if (keysRes.data) setRegisteredKeys(keysRes.data);
      showToast(`Exchange ${popupConfig.exchange} updated.`, 'success');
    } catch (err) {
      showToast('Action failed: ' + (err.response?.data?.error || err.message), 'error');
    } finally {
      setPopupConfig(null);
      setSaving(false);
    }
  };

  const isCloseToExpiry = (expiryDateStr) => {
    if (!expiryDateStr) return false;
    const expDate = new Date(expiryDateStr);
    const timeDiff = expDate.getTime() - new Date().getTime();
    const daysDiff = Math.ceil(timeDiff / (1000 * 3600 * 24));
    return daysDiff <= 7 && daysDiff > 0;
  };

  const handleSaveParams = async (e) => {
    e.preventDefault();
    setSaving(true);
    try {
      await apiClient.post('/settings/parameters', {
        StrategyName: 'PremiumStrategy',
        UsePercentParams: params.usePercentParams,
        MinTargetProfit: parseFloat(params.minTargetProfitUsd),
        MaxStopLoss: parseFloat(params.maxStopLossUsd),
        MinTargetProfitPercent: parseFloat(params.minTargetProfitPct),
        MaxStopLossPercent: parseFloat(params.maxStopLossPct),
        MaxAcceptableSpread: parseFloat(params.maxAcceptableSpread),
        CapitalLimitUsd: parseFloat(params.capitalLimitUsd),
        MaxRiskScore: parseFloat(params.maxRiskScore),
        RiskMultiplier: parseFloat(params.riskMultiplier),
        RebalanceTriggerSkew: parseFloat(params.rebalanceTriggerSkewPct) / 100.0,
        RebalanceTargetSkew: parseFloat(params.rebalanceTargetSkewPct) / 100.0,
        RebalanceMaxPremium: parseFloat(params.rebalanceMaxPremiumPct) / 100.0
      });
      showToast('Trading & Auto-Rebalancing parameters updated!', 'success');
    } catch (err) {
      showToast('Failed to save parameters.', 'error');
    } finally {
      setSaving(false);
    }
  };

  const handleSaveProfile = async (e) => {
    e.preventDefault();
    setSaving(true);
    try {
      await apiClient.post('/settings/profile', {
        NotifyOnEntry: profile.notifyOnEntry,
        NotifyOnExit: profile.notifyOnExit,
        DiscordWebhookURL: profile.discordWebhook,
        TelegramChatID: profile.telegramChatId,
        PartialExitMode: profile.partialExitMode,
        PartialExitRatio: parseFloat(profile.partialExitRatio),
        EnableStrategicSwitching: profile.enableStrategicSwitching,
        SwitchingProfitMultiplier: parseFloat(profile.switchingProfitMultiplier)
      });
      showToast('Profile & AI Portfolio Settings updated!', 'success');
    } catch (err) {
      showToast('Failed to save profile settings.', 'error');
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="flex-center" style={{ height: '100%' }}><div className="logo-orb animate-pulse"></div></div>;

  return (
    <div className="settings-page animate-fade-in delay-100">
      <header className="page-header mb-lg">
        <h1 className="text-2xl font-bold">Trading Configuration</h1>
        <p className="text-secondary mt-xs">Manage your exchange connections, delta-neutral rebalancing parameters, and notifications.</p>
      </header>

      <div className="settings-layout">
        {/* Sidebar Navigation */}
        <div className="settings-sidebar glass-panel">
          <nav className="settings-nav">
            <button className={`settings-tab ${activeTab === 'exchange' ? 'active' : ''}`} onClick={() => setActiveTab('exchange')}>
              <Key size={18} /> Exchange Connections
            </button>
            <button className={`settings-tab ${activeTab === 'params' ? 'active' : ''}`} onClick={() => setActiveTab('params')}>
              <Sliders size={18} /> Trade & Skew Parameters
            </button>
            <button className={`settings-tab ${activeTab === 'profile' ? 'active' : ''}`} onClick={() => setActiveTab('profile')}>
              <Bell size={18} /> Webhooks & AI Portfolio
            </button>
          </nav>
        </div>

        {/* Tab Content Area */}
        <div className="settings-content">
          {activeTab === 'exchange' && (
            <div className="settings-card glass-panel p-xl animate-fade-in">
              <div className="flex-between mb-md">
                <h2 className="settings-h2 text-xl font-semibold m-0">Exchange Connections</h2>
                {registeredKeys.length > 0 && (
                  <button className="btn-secondary text-sm px-sm py-1 rounded border border-white/20 hover:bg-white/10" onClick={() => setEditMode(!editMode)}>
                    {editMode ? 'Done' : 'Edit'}
                  </button>
                )}
              </div>

              {registeredKeys.length < 2 && (
                <div className="alert-warning mb-md flex items-center gap-sm text-sm p-sm rounded bg-amber-500/10 border border-amber-500/20 text-amber-200">
                  <AlertCircle size={16} className="text-amber-500" />
                  Arbitrage trading requires at least 2 registered exchanges to function.
                </div>
              )}

              <div className="exchange-status-cards mb-xl flex flex-col gap-md">
                {registeredKeys.map((ex) => (
                  <div key={ex.exchange} className={`status-card flex items-start justify-between ${ex.is_active ? 'active-border' : ''}`}>
                    <div className="flex flex-col">
                      <div className="flex items-center gap-md">
                        <div className="font-bold text-lg">{ex.exchange}</div>
                        {ex.is_active ? (
                          <span className="text-xs px-sm py-1 rounded bg-green-500/20 text-green-400 font-semibold border border-green-500/30">Active</span>
                        ) : (
                          <span className="text-xs px-sm py-1 rounded bg-gray-500/20 text-gray-400 font-semibold border border-gray-500/30">Paused</span>
                        )}
                      </div>

                      {!ex.is_valid && (
                        <div className="text-red-400 text-xs mt-2 flex items-center gap-1">
                          <XCircle size={14} className="flex-shrink-0" /> {ex.error_msg || 'API Key is invalid or lacks necessary permissions.'}
                        </div>
                      )}
                      {ex.expires_at && isCloseToExpiry(ex.expires_at) && (
                        <div className="text-amber-400 text-xs mt-2 flex items-center gap-1">
                          <Clock size={14} className="flex-shrink-0" /> API Key expires on {ex.expires_at}. Please renew soon.
                        </div>
                      )}
                    </div>

                    <div className="flex items-center self-center gap-3">
                      {editMode ? (
                        <button
                          className="text-red-400 hover:text-red-300 transition-colors p-2 rounded-full hover:bg-white/5"
                          onClick={() => handleDeleteExchange(ex.exchange)}
                          title="Remove Exchange"
                        >
                          <X size={20} />
                        </button>
                      ) : (
                        <Toggle
                          checked={ex.is_active}
                          onChange={(e) => handleToggleExchange(ex.exchange, e.target.checked)}
                        />
                      )}
                    </div>
                  </div>
                ))}

                {registeredKeys.length === 0 && (
                  <div className="text-center p-xl text-secondary border border-dashed border-white/10 rounded-xl">
                    No exchanges registered yet. Add at least 2 below to begin trading.
                  </div>
                )}
              </div>

              <div className="separator mb-lg"></div>

              <form onSubmit={handleSaveKeys} className="flex flex-col">
                <h3 className="text-lg font-semibold mb-lg">Register New Exchange</h3>

                <div className="form-group w-full max-w-sm">
                  <label className="text-sm text-secondary">Exchange</label>
                  <select
                    value={apiKeys.exchange}
                    onChange={e => setApiKeys({ ...apiKeys, exchange: e.target.value })}
                    className="form-input mt-xs"
                  >
                    {supportedExchanges.map(ex => (
                      <option key={ex} value={ex}>{ex}</option>
                    ))}
                  </select>
                </div>

                <div className="form-group">
                  <label className="text-sm text-secondary">Access Key / API Name</label>
                  <input
                    type="text"
                    value={apiKeys.access_key}
                    onChange={e => setApiKeys({ ...apiKeys, access_key: e.target.value })}
                    className="form-input mt-xs"
                    required
                    placeholder="Enter your public access key"
                  />
                </div>

                <div className="form-group">
                  <label className="text-sm text-secondary">Secret Key (Encrypted at Rest)</label>
                  <p className="form-help-text mb-sm">Must have Read, Trade, and Withdrawal permissions allowed for the engine.</p>
                  <input
                    type="password"
                    value={apiKeys.secret_key}
                    onChange={e => setApiKeys({ ...apiKeys, secret_key: e.target.value })}
                    className="form-input mt-xs"
                    required
                    placeholder="Enter your secret key"
                  />
                </div>

                <button type="submit" disabled={saving} className="btn-primary self-start">
                  <Save size={18} /> {saving ? 'Verifying...' : 'Verify Keys & Add Exchange'}
                </button>
              </form>

              <div className="separator my-lg"></div>

              <div className="flex flex-col gap-sm">
                <div className="flex-between">
                  <div>
                    <h3 className="text-lg font-semibold m-0">Web Coin Lending Session (noVNC Browser)</h3>
                    <p className="text-xs text-secondary mt-xs">
                      For exchanges without official Margin Borrow APIs (e.g., Upbit Coin Lending, Bithumb), authenticate directly inside the isolated browser container via QR / 2FA. The engine watchdog continuously monitors and maintains this session.
                    </p>
                  </div>
                  <button
                    type="button"
                    className="btn-secondary text-sm px-md py-2 rounded border border-white/20 hover:bg-white/10"
                    onClick={() => setShowNoVNCModal(true)}
                  >
                    Open noVNC Browser Login
                  </button>
                </div>
              </div>
            </div>
          )}

          {activeTab === 'params' && (
            <div className="settings-card glass-panel p-xl animate-fade-in">
              <h2 className="settings-h2 text-xl font-semibold mb-lg">Arbitrage & Risk Parameters</h2>
              <form onSubmit={handleSaveParams} className="flex flex-col max-w-lg">
                <div className="flex-between mb-lg">
                  <div>
                    <h4 className="font-semibold text-sm">Use Percentage Parameters</h4>
                    <p className="text-xs text-muted mt-xs">Toggle to define thresholds by percentage of total investment rather than absolute USD value.</p>
                  </div>
                  <Toggle checked={params.usePercentParams} onChange={e => setParams({ ...params, usePercentParams: e.target.checked })} />
                </div>

                <div className="separator mb-lg"></div>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold block mb-xs">
                    Min Target Profit {params.usePercentParams ? '(%)' : '(USD)'}
                  </label>
                  <p className="form-help-text mb-sm">Only execute trades if the net ROI exceeds this amount after fees.</p>
                  {params.usePercentParams ? (
                    <input
                      type="number" step="0.1" min="0" required
                      value={params.minTargetProfitPct}
                      onChange={e => setParams({ ...params, minTargetProfitPct: e.target.value })}
                      className="form-input w-full max-w-xs"
                    />
                  ) : (
                    <input
                      type="number" step="1" min="1" required
                      value={params.minTargetProfitUsd}
                      onChange={e => setParams({ ...params, minTargetProfitUsd: e.target.value })}
                      className="form-input w-full max-w-xs"
                    />
                  )}
                </div>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold block mb-xs">
                    Max Stop Loss {params.usePercentParams ? '(%)' : '(USD)'}
                  </label>
                  <p className="form-help-text mb-sm">Emergency liquidation threshold if positions move against the expected premium prediction.</p>
                  {params.usePercentParams ? (
                    <input
                      type="number" step="0.1" max="-0.1" required
                      value={params.maxStopLossPct}
                      onChange={e => setParams({ ...params, maxStopLossPct: e.target.value })}
                      className="form-input w-full max-w-xs"
                      placeholder="-10.0"
                    />
                  ) : (
                    <input
                      type="number" step="1" min="1" required
                      value={params.maxStopLossUsd}
                      onChange={e => setParams({ ...params, maxStopLossUsd: e.target.value })}
                      className="form-input w-full max-w-xs"
                    />
                  )}
                </div>

                <div className="form-group">
                  <label className="text-sm font-semibold block mb-xs">Max Acceptable Slippage (Spread %)</label>
                  <p className="form-help-text mb-sm">Maximum allowed difference between expected quote and actual executed market fill.</p>
                  <input
                    type="number" step="0.001" min="0" required
                    value={params.maxAcceptableSpread}
                    onChange={e => setParams({ ...params, maxAcceptableSpread: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold block mb-xs">Capital Allocation Limit (USD)</label>
                  <p className="form-help-text mb-sm">Maximum USD equivalent of exposure the bot is allowed to allocate simultaneously.</p>
                  <input
                    type="number" step="1" min="10" required
                    value={params.capitalLimitUsd}
                    onChange={e => setParams({ ...params, capitalLimitUsd: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold block mb-xs">Max AI Risk Score</label>
                  <p className="form-help-text mb-sm">Opportunities yielding a Risk Score higher than this limit are ignored.</p>
                  <input
                    type="number" step="0.1" min="1" max="100" required
                    value={params.maxRiskScore}
                    onChange={e => setParams({ ...params, maxRiskScore: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <div className="form-group mb-lg">
                  <label className="text-sm font-semibold block mb-xs">Transfer Time Risk Multiplier</label>
                  <p className="form-help-text mb-sm">Amplifies the base AI Network Risk Score based on estimated blockchain confirmation delays.</p>
                  <input
                    type="number" step="0.1" min="0" required
                    value={params.riskMultiplier}
                    onChange={e => setParams({ ...params, riskMultiplier: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <div className="separator mb-lg"></div>

                {/* 100% Delta-Neutral Auto-Rebalancing Section */}
                <h3 className="text-lg font-semibold mb-sm flex items-center gap-sm">
                  <Scale size={18} className="text-blue-400" /> 100% Delta-Neutral Capital Rebalancing
                </h3>
                <p className="form-help-text mb-lg">
                  When Transfer Arbitrage (Strategy C) shifts quote capital from Foreign (USD) to Base (KRW), the Engine automatically executes a hedged reverse transfer (Base Spot Buy + Foreign 1x Short → Transfer → Foreign Sell + Close Short) once the Dual-Trigger conditions below are met.
                </p>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold block mb-xs">Rebalance Trigger Foreign Share (%)</label>
                  <p className="form-help-text mb-sm">Trigger auto-rebalancing when Foreign USD balance falls below this percentage of total portfolio equity (Default: 20%).</p>
                  <input
                    type="number" step="1" min="5" max="45" required
                    value={params.rebalanceTriggerSkewPct}
                    onChange={e => setParams({ ...params, rebalanceTriggerSkewPct: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold block mb-xs">Rebalance Target Foreign Share (%)</label>
                  <p className="form-help-text mb-sm">Target Foreign USD share to restore during hedged reverse transfer (Default: 50% for 50:50 balance).</p>
                  <input
                    type="number" step="1" min="30" max="70" required
                    value={params.rebalanceTargetSkewPct}
                    onChange={e => setParams({ ...params, rebalanceTargetSkewPct: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <div className="form-group mb-lg">
                  <label className="text-sm font-semibold block mb-xs">Max Rebalance Spread / Premium (%)</label>
                  <p className="form-help-text mb-sm">Only execute reverse transfer when the Base-vs-Foreign premium is at or below this threshold (e.g., 0.0% break-even or negative reverse premium).</p>
                  <input
                    type="number" step="0.1" min="-5.0" max="2.0" required
                    value={params.rebalanceMaxPremiumPct}
                    onChange={e => setParams({ ...params, rebalanceMaxPremiumPct: e.target.value })}
                    className="form-input w-full max-w-xs"
                  />
                </div>

                <button type="submit" disabled={saving} className="btn-primary self-start">
                  <Save size={18} /> {saving ? 'Saving...' : 'Update Parameters'}
                </button>
              </form>
            </div>
          )}

          {activeTab === 'profile' && (
            <div className="settings-card glass-panel p-xl animate-fade-in">
              <h2 className="settings-h2 text-xl font-semibold mb-lg">Alerts & Notifications</h2>
              <form onSubmit={handleSaveProfile} className="flex flex-col max-w-lg">
                <div className="flex flex-col gap-xl mb-xl">
                  <div className="form-group flex-between">
                    <div>
                      <h4 className="font-semibold text-sm">Notify on Position Entry</h4>
                      <p className="text-xs text-muted mt-xs">Receive a webhook when the bot enters a new position.</p>
                    </div>
                    <Toggle checked={profile.notifyOnEntry} onChange={e => setProfile({ ...profile, notifyOnEntry: e.target.checked })} />
                  </div>

                  <div className="form-group flex-between">
                    <div>
                      <h4 className="font-semibold text-sm">Notify on Position Exit</h4>
                      <p className="text-xs text-muted mt-xs">Receive a webhook when a position is closed or liquidated.</p>
                    </div>
                    <Toggle checked={profile.notifyOnExit} onChange={e => setProfile({ ...profile, notifyOnExit: e.target.checked })} />
                  </div>
                </div>

                <div className="separator mb-lg"></div>

                <h2 className="settings-h2 text-xl font-semibold mb-lg mt-md">AI Portfolio Management</h2>

                <div className="flex flex-col gap-xl mb-xl">
                  <div className="form-group">
                    <div className="flex-between mb-xs">
                      <h4 className="font-semibold text-sm">Partial Exit (Scale-Out) Mode</h4>
                    </div>
                    <p className="form-help-text mb-sm">
                      Should the bot sell a fixed fraction on a SCALE_OUT signal, or dynamically reduce weight based on the AI target weight?
                    </p>
                    <select
                      value={profile.partialExitMode}
                      onChange={e => setProfile({ ...profile, partialExitMode: e.target.value })}
                      className="form-input mt-xs w-full max-w-sm"
                    >
                      <option value="FIXED">FIXED (Constant Fraction)</option>
                      <option value="AUTO">AUTO (Dynamic AI Proportional)</option>
                    </select>
                  </div>

                  {profile.partialExitMode === 'FIXED' && (
                    <div className="form-group pl-lg border-l-2 border-primary/30">
                      <label className="text-sm font-semibold text-secondary block mb-xs">Fixed Exit Ratio</label>
                      <p className="form-help-text mb-sm">A value of 0.5 means selling 50% of the active position when commanded.</p>
                      <input
                        type="number" step="0.1" min="0.1" max="1.0"
                        value={profile.partialExitRatio}
                        onChange={e => setProfile({ ...profile, partialExitRatio: e.target.value })}
                        className="form-input w-full max-w-xs"
                      />
                    </div>
                  )}

                  <div className="form-group mt-md">
                    <div className="flex-between mb-xs">
                      <div>
                        <h4 className="font-semibold text-sm">Enable Strategic Opportunity Switching</h4>
                        <p className="text-xs text-muted mt-xs">Allow the AI to rotate capital early if a vastly superior arbitrage opportunity is discovered globally.</p>
                      </div>
                      <Toggle checked={profile.enableStrategicSwitching} onChange={e => setProfile({ ...profile, enableStrategicSwitching: e.target.checked })} />
                    </div>
                  </div>

                  {profile.enableStrategicSwitching && (
                    <div className="form-group pl-lg border-l-2 border-primary/30">
                      <label className="text-sm font-semibold text-secondary block mb-xs">Switching Profit Multiplier (Gap Requirement)</label>
                      <p className="form-help-text mb-sm">
                        Only switch if the new opportunity's ROI is at least X times higher than the current position. e.g. 2.0x
                      </p>
                      <input
                        type="number" step="0.5" min="1.1"
                        value={profile.switchingProfitMultiplier}
                        onChange={e => setProfile({ ...profile, switchingProfitMultiplier: e.target.value })}
                        className="form-input w-full max-w-xs"
                      />
                    </div>
                  )}
                </div>

                <div className="separator mb-lg"></div>

                <div className="form-group mb-md">
                  <label className="text-sm font-semibold text-secondary">Discord Webhook URL</label>
                  <input
                    type="url"
                    value={profile.discordWebhook}
                    onChange={e => setProfile({ ...profile, discordWebhook: e.target.value })}
                    className="form-input mt-xs w-full"
                    placeholder="https://discord.com/api/webhooks/..."
                  />
                </div>

                <div className="form-group">
                  <label className="text-sm font-semibold text-secondary">Telegram Chat ID</label>
                  <input
                    type="text"
                    value={profile.telegramChatId}
                    onChange={e => setProfile({ ...profile, telegramChatId: e.target.value })}
                    className="form-input mt-xs w-full"
                    placeholder="-10012345678"
                  />
                </div>

                <button type="submit" disabled={saving} className="btn-primary self-start">
                  <Save size={18} /> {saving ? 'Saving...' : 'Update Preferences'}
                </button>
              </form>
            </div>
          )}
        </div>
      </div>

      {/* Popups */}
      {popupConfig && (
        <div className="fixed inset-0 bg-black/70 flex-center z-50">
          <div className="bg-gray-900 border border-gray-700 rounded-xl p-xl max-w-sm w-full animate-fade-in">
            <h3 className="text-xl font-bold mb-md text-amber-400 flex items-center gap-2">
              <AlertTriangle size={24} /> Action Required
            </h3>
            <p className="text-sm text-gray-300 mb-lg">
              You currently hold active positional exposure dependent on <strong>{popupConfig.exchange}</strong>. How would you like to proceed?
            </p>

            {popupConfig.type === 'delete' && (
              <div className="flex flex-col gap-sm">
                <button
                  className="btn-primary w-full justify-center bg-red-600 hover:bg-red-700"
                  onClick={() => confirmAction(true, 'close_and_remove')}
                >
                  Close all positions and remove
                </button>
                <button
                  className="btn-secondary w-full justify-center"
                  onClick={() => setPopupConfig(null)}
                >
                  Keep Exchange (Cancel)
                </button>
              </div>
            )}

            {popupConfig.type === 'toggle' && (
              <div className="flex flex-col gap-sm">
                <button
                  className="btn-primary w-full justify-center bg-red-600 hover:bg-red-700"
                  onClick={() => confirmAction(true, 'close_and_disable')}
                >
                  Close all positions and disable
                </button>
                <button
                  className="btn-secondary w-full justify-center border-amber-600 hover:bg-amber-600/20 text-amber-500"
                  onClick={() => confirmAction(true, 'keep_and_disable')}
                >
                  Keep positions and disable (Risky)
                </button>
                <button
                  className="btn-secondary w-full justify-center"
                  onClick={() => setPopupConfig(null)}
                >
                  Keep Active (Cancel)
                </button>
              </div>
            )}
          </div>
        </div>
      )}

      {/* noVNC Browser Session Modal */}
      {showNoVNCModal && (
        <div className="fixed inset-0 bg-black/80 flex-center z-50 p-lg">
          <div className="bg-gray-900 border border-gray-700 rounded-xl p-lg max-w-5xl w-full flex flex-col gap-md animate-fade-in" style={{ height: '80vh' }}>
            <div className="flex-between">
              <div>
                <h3 className="text-lg font-bold m-0">Exchange Web Lending Session (noVNC)</h3>
                <p className="text-xs text-secondary m-0">Complete QR / 2FA login below. Once authenticated, the Go Engine CDP Watchdog keeps your lending session alive.</p>
              </div>
              <div className="flex items-center gap-sm">
                <input
                  type="text"
                  value={noVNCUrl}
                  onChange={e => setNoVNCUrl(e.target.value)}
                  className="form-input text-xs px-sm py-1"
                  style={{ width: '220px' }}
                />
                <button className="btn-secondary p-2 rounded" onClick={() => setShowNoVNCModal(false)}>
                  <X size={18} />
                </button>
              </div>
            </div>
            <iframe
              src={noVNCUrl}
              title="noVNC Exchange Session"
              className="w-full flex-1 rounded border border-white/10 bg-black"
            />
          </div>
        </div>
      )}
    </div>
  );
};
