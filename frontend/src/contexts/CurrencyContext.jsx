import React, { createContext, useContext, useState, useCallback, useMemo } from 'react';

export const SUPPORTED_CURRENCIES = {
  USD: {
    code: 'USD',
    symbol: '$',
    labelKo: 'USD ($)',
    labelEn: 'USD ($)',
    locale: 'en-US',
    fractionDigits: 2,
    defaultRatePerUSD: 1.0,
  },
  KRW: {
    code: 'KRW',
    symbol: '₩',
    labelKo: 'KRW (₩)',
    labelEn: 'KRW (₩)',
    locale: 'ko-KR',
    fractionDigits: 0,
    defaultRatePerUSD: 1400.0,
  },
};

const CurrencyContext = createContext(null);

export const CurrencyProvider = ({ children }) => {
  const [currency, setCurrencyState] = useState(() => {
    try {
      const saved = localStorage.getItem('magnolia_currency');
      if (saved && SUPPORTED_CURRENCIES[saved]) return saved;
    } catch {
      // ignore storage errors
    }
    return 'USD';
  });

  const [fxRates, setFxRates] = useState({
    USD: 1.0,
    KRW: 1400.0,
  });

  const setCurrency = useCallback((nextCode) => {
    const code = String(nextCode || 'USD').toUpperCase();
    if (!SUPPORTED_CURRENCIES[code]) return;
    setCurrencyState(code);
    try {
      localStorage.setItem('magnolia_currency', code);
    } catch {
      // ignore storage errors
    }
  }, []);

  const updateFxRate = useCallback((code, ratePerUSD) => {
    const num = Number(ratePerUSD);
    if (!code || !Number.isFinite(num) || num <= 0) return;
    const upper = String(code).toUpperCase();
    setFxRates((prev) => {
      if (Math.abs((prev[upper] || 0) - num) < 0.01) return prev;
      return { ...prev, [upper]: num };
    });
  }, []);

  const convertFromUSD = useCallback(
    (usdAmount, targetCode = currency, customKrwRate = null) => {
      const num = Number(usdAmount || 0);
      const code = String(targetCode || currency || 'USD').toUpperCase();
      const meta = SUPPORTED_CURRENCIES[code] || SUPPORTED_CURRENCIES.USD;
      if (code === 'USD') return num;
      if (code === 'KRW' && customKrwRate && Number(customKrwRate) > 0) {
        return num * Number(customKrwRate);
      }
      const rate = fxRates[code] || meta.defaultRatePerUSD || 1.0;
      return num * rate;
    },
    [currency, fxRates]
  );

  const formatMoney = useCallback(
    (usdAmount, options = {}) => {
      const {
        currencyOverride,
        customKrwRate = null,
        signed = false,
        fractionDigitsOverride = null,
      } = options;
      const code = String(currencyOverride || currency || 'USD').toUpperCase();
      const meta = SUPPORTED_CURRENCIES[code] || SUPPORTED_CURRENCIES.USD;
      const converted = convertFromUSD(usdAmount, code, customKrwRate);
      const absVal = Math.abs(converted);
      const digits =
        fractionDigitsOverride !== null ? fractionDigitsOverride : meta.fractionDigits;
      const formatted = absVal.toLocaleString(meta.locale, {
        minimumFractionDigits: digits,
        maximumFractionDigits: digits,
      });
      if (signed) {
        const sign = converted >= 0 ? '+' : '-';
        return `${sign}${meta.symbol}${formatted}`;
      }
      return converted < 0 ? `-${meta.symbol}${formatted}` : `${meta.symbol}${formatted}`;
    },
    [currency, convertFromUSD]
  );

  const formatNative = useCallback((nativeAmount, quoteCurrency = 'USD') => {
    const code = String(quoteCurrency || 'USD').toUpperCase();
    const num = Number(nativeAmount || 0);
    if (code === 'KRW') {
      return `₩${Math.round(num).toLocaleString('ko-KR')}`;
    }
    return `$${num.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
  }, []);

  const value = useMemo(
    () => ({
      currency,
      setCurrency,
      supportedCurrencies: Object.values(SUPPORTED_CURRENCIES),
      fxRates,
      updateFxRate,
      convertFromUSD,
      formatMoney,
      formatNative,
    }),
    [currency, setCurrency, fxRates, updateFxRate, convertFromUSD, formatMoney, formatNative]
  );

  return <CurrencyContext.Provider value={value}>{children}</CurrencyContext.Provider>;
};

export const useCurrency = () => {
  const ctx = useContext(CurrencyContext);
  if (!ctx) {
    // Fallback for isolated component tests that don't wrap in CurrencyProvider
    return {
      currency: 'USD',
      setCurrency: () => {},
      supportedCurrencies: Object.values(SUPPORTED_CURRENCIES),
      fxRates: { USD: 1.0, KRW: 1400.0 },
      updateFxRate: () => {},
      convertFromUSD: (v) => Number(v || 0),
      formatMoney: (v, opts = {}) => {
        const n = Number(v || 0);
        const abs = Math.abs(n).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
        if (opts.signed) return `${n >= 0 ? '+' : '-'}$${abs}`;
        return n < 0 ? `-$${abs}` : `$${abs}`;
      },
      formatNative: (v, q = 'USD') =>
        String(q).toUpperCase() === 'KRW'
          ? `₩${Math.round(Number(v || 0)).toLocaleString('ko-KR')}`
          : `$${Number(v || 0).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`,
    };
  }
  return ctx;
};
