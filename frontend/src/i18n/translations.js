export const translations = {
  ko: {
    nav: {
      dashboard: '대시보드',
      market: '마켓 & 시뮬레이터',
      settings: '설정 & 거래이력',
      paperMode: '모의매매',
      liveMode: '실매매',
      botRunning: '자동매매 가동 중',
      botDraining: '신규 진입 중단 (정리 중)',
      botStopped: '자동매매 중지됨',
      startBot: '자동매매 시작',
      stopBot: '자동매매 중지',
      logout: '로그아웃'
    },
    dashboard: {
      titlePaper: '모의매매 자산 및 포지션 현황',
      titleLive: '실매매 자산 및 포지션 현황',
      subPaper: '실제 업비트·코인베이스 실시간 호가를 기반으로 가상 자본이 운용됩니다.',
      subLive: '연결된 거래소 API를 통해 실제 델타 중립 아비트라지 포지션을 관리합니다.',
      totalEquity: '총 평가자산',
      upbitCash: '업비트 주문 가능 원화',
      coinbaseCash: '코인베이스 주문 가능 달러',
      capitalBalance: '거래소 자본 비중 (업비트 : 코인베이스)',
      rebalanceActive: '역전송 리밸런싱 대상',
      rebalanceNormal: '균형 안정',
      webLendingAlertTitle: '업비트 웹 코인 대여 세션 연결이 필요합니다',
      webLendingAlertDesc: '김프 마진 숏 및 전송 차익 전략을 실행하려면 웹 브라우저 대여 세션이 유지되어야 합니다.',
      webLendingConnectBtn: '웹 대여 브라우저 열기',
      activePositionsTitle: '보유 중인 포지션',
      noPositionsTitle: '현재 열려 있는 포지션이 없습니다.',
      noPositionsDesc: 'AI가 목표 수익률 이상의 프리미엄 기회를 포착하면 자동으로 진입하거나, 마켓 탭에서 직접 진입할 수 있습니다.',
      goToMarketBtn: '실시간 마켓 & 시뮬레이터 보기',
      colAsset: '자산 및 경로',
      colStrategy: '운용 전략',
      colInvested: '투입 자본',
      colEntrySpread: '진입 프리미엄',
      colPnl: '현재 평가손익',
      colStatus: '진행 상태',
      colAction: '관리',
      closePositionBtn: '즉시 청산',
      confirmCloseTitle: '포지션 즉시 청산',
      confirmCloseDesc: '양쪽 거래소(롱·숏) 포지션을 동시에 시장가로 정리하시겠습니까?'
    },
    market: {
      title: '실시간 글로벌 프리미엄 & 시뮬레이터',
      subtitle: '업비트와 코인베이스의 실시간 호가 격차(김치 프리미엄)와 8시간 AI 예측 궤적을 비교합니다.',
      simTitle: '모의매매 실시간 시세 오프셋 시뮬레이터',
      simDesc: '실제 거래소 호가에 가상 프리미엄(±N%)을 X분 동안 부드럽게(S-커브) 더해 AI와 엔진의 자동 진입·청산을 검증합니다.',
      simExchange: '대상 거래소',
      simTargetPct: '목표 오프셋 (%)',
      simRampMin: '도달 시간 (분)',
      simApplyBtn: '시뮬레이션 적용',
      simPresetKimchi: '김프 +3.5% (3분)',
      simPresetDiscount: '역프 -1.5% (2분)',
      simPresetInstant: '즉시 +4.0% (0분)',
      simPresetReset: '실제 시세로 초기화 (0%)',
      simCurrentStatus: '현재 적용 오프셋',
      tableTitle: '거래소 간 실시간 프리미엄 목록',
      colPair: '종목 (기준 → 해외)',
      colBasePrice: '기준 거래소 호가',
      colForeignPrice: '해외 거래소 호가',
      colSpread: '현재 프리미엄',
      colAiForecast: 'AI 8시간 예측',
      colRecommended: '추천 전략',
      colNetMargin: '예상 순마진',
      colManual: '수동 실행',
      enterBtn: '포지션 진입',
      noOpps: '실시간 시세 스트림을 수신하는 중입니다...'
    },
    settings: {
      title: '설정 및 거래 이력',
      subtitle: '거래소 API 연결, 자동매매 리스크 기준, 모의매매 가상 시드 및 과거 청산 내역을 관리합니다.',
      tabExchange: '거래소 연결 & 웹 대여',
      tabParams: '자동매매 기준 설정',
      tabPaper: '모의매매 시드 관리',
      tabHistory: '과거 거래 이력',
      exchangeListTitle: '연결된 거래소 API 목록',
      openNoVncBtn: '업비트 웹 대여 브라우저 (noVNC)',
      addExchangeTitle: '거래소 API 키 등록 / 변경',
      exchangeLabel: '거래소 선택',
      accessKeyLabel: 'Access Key (API 키)',
      secretKeyLabel: 'Secret Key (비밀키)',
      saveKeyBtn: 'API 키 암호화 저장',
      paramsTitle: '수익률 및 리스크 관리 기준',
      minProfitLabel: '최소 목표 수익률 (%)',
      maxLossLabel: '최대 허용 손절 폭 (%)',
      capitalLimitLabel: '1회 최대 진입 한도 (USD)',
      maxSpreadLabel: '허용 호가 슬리피지 한도',
      rebalanceTriggerLabel: '역전송 리밸런싱 발동 기준 (해외 USD 비중 % 이하)',
      rebalanceTargetLabel: '리밸런싱 목표 복구 비중 (해외 USD %)',
      rebalanceMaxPremLabel: '리밸런싱 허용 최대 프리미엄 (%)',
      saveParamsBtn: '설정 저장하기',
      paperTitle: '모의매매 가상 계좌 시드 설정',
      paperDesc: '모의매매 초기 자본금을 변경하고 열려 있는 모든 가상 포지션을 초기화합니다.',
      paperUpbitKrwLabel: '업비트 초기 가상 원화 (KRW)',
      paperCoinbaseUsdLabel: '코인베이스 초기 가상 달러 (USD)',
      resetPaperBtn: '모의매매 계좌 즉시 초기화',
      historyTitle: '완료된 아비트라지 거래 내역',
      exportCsvBtn: 'CSV 다운로드',
      colOpenedAt: '진입 시각',
      colClosedAt: '종료 시각',
      colRealizedPnl: '확정 손익',
      noHistory: '아직 완료된 거래 내역이 없습니다.'
    },
    stopModal: {
      title: '보유 중인 포지션 처리 방식을 선택해 주세요',
      desc: '현재 열려 있는 아비트라지 포지션이 있습니다. 봇을 어떻게 중지할까요?',
      optDrainTitle: '신규 진입만 중단 (기존 포지션은 익절/청산 시까지 자동 관리)',
      optDrainDesc: '가장 안전한 권장 방식입니다. 열린 포지션이 목표 수익에 도달하면 자동으로 종료됩니다.',
      optCloseTitle: '즉시 모든 포지션 시장가 청산 후 봇 완전 정지',
      optCloseDesc: '현재 열려 있는 모든 포지션을 즉시 시장가로 정리하고 봇을 멈춥니다.',
      cancel: '취소'
    }
  },
  en: {
    nav: {
      dashboard: 'Dashboard',
      market: 'Market & Simulator',
      settings: 'Settings & History',
      paperMode: 'Paper Mode',
      liveMode: 'Live Mode',
      botRunning: 'Bot Running',
      botDraining: 'Draining',
      botStopped: 'Bot Stopped',
      startBot: 'Start Bot',
      stopBot: 'Stop Bot',
      logout: 'Sign Out'
    },
    dashboard: {
      titlePaper: 'Paper Portfolio & Active Positions',
      titleLive: 'Live Portfolio & Active Positions',
      subPaper: 'Simulated execution powered by 100% real-time Upbit & Coinbase orderbooks.',
      subLive: 'Delta-neutral cross-exchange arbitrage execution via connected APIs.',
      totalEquity: 'Total Equity',
      upbitCash: 'Upbit Available KRW',
      coinbaseCash: 'Coinbase Available USD',
      capitalBalance: 'Capital Split (Upbit : Coinbase)',
      rebalanceActive: 'Rebalance Needed',
      rebalanceNormal: 'Balanced',
      webLendingAlertTitle: 'Upbit Web Lending Session Required',
      webLendingAlertDesc: 'Please authenticate the noVNC browser session to enable margin short & transfer arbitrage.',
      webLendingConnectBtn: 'Open Web Lending Browser',
      activePositionsTitle: 'Active Positions',
      noPositionsTitle: 'No open arbitrage positions.',
      noPositionsDesc: 'The engine will automatically enter when spread exceeds your target threshold, or you can enter manually from the Market tab.',
      goToMarketBtn: 'Go to Market & Simulator',
      colAsset: 'Pair & Route',
      colStrategy: 'Strategy',
      colInvested: 'Capital',
      colEntrySpread: 'Entry Spread',
      colPnl: 'Unrealized PnL',
      colStatus: 'Status',
      colAction: 'Action',
      closePositionBtn: 'Close Now',
      confirmCloseTitle: 'Close Position Immediately',
      confirmCloseDesc: 'Unwind both long and short legs at market price right now?'
    },
    market: {
      title: 'Global Premium Scanner & Simulator',
      subtitle: 'Real-time cross-exchange spread compared against 8-hour AI trajectory predictions.',
      simTitle: 'Paper Price Offset Simulator',
      simDesc: 'Gradually apply a virtual ±N% offset over X minutes (S-curve) to real exchange quotes to test bot entry and exit.',
      simExchange: 'Exchange',
      simTargetPct: 'Target Offset (%)',
      simRampMin: 'Ramp Duration (min)',
      simApplyBtn: 'Apply Offset',
      simPresetKimchi: 'Kimchi +3.5% (3m)',
      simPresetDiscount: 'Reverse -1.5% (2m)',
      simPresetInstant: 'Instant +4.0% (0m)',
      simPresetReset: 'Reset to Real Price (0%)',
      simCurrentStatus: 'Current Offset',
      tableTitle: 'Live Cross-Exchange Opportunities',
      colPair: 'Asset (Base → Foreign)',
      colBasePrice: 'Base Price',
      colForeignPrice: 'Foreign Price',
      colSpread: 'Current Spread',
      colAiForecast: 'AI 8h Forecast',
      colRecommended: 'Strategy',
      colNetMargin: 'Est. Net Margin',
      colManual: 'Action',
      enterBtn: 'Enter Trade',
      noOpps: 'Waiting for real-time market stream...'
    },
    settings: {
      title: 'Settings & Trade History',
      subtitle: 'Manage exchange API keys, risk thresholds, paper trading seed, and closed trade history.',
      tabExchange: 'Exchanges & Web Lending',
      tabParams: 'Trading Thresholds',
      tabPaper: 'Paper Seed Reset',
      tabHistory: 'Trade History',
      exchangeListTitle: 'Connected Exchange APIs',
      openNoVncBtn: 'Upbit Web Lending (noVNC)',
      addExchangeTitle: 'Register / Update API Key',
      exchangeLabel: 'Exchange',
      accessKeyLabel: 'Access Key',
      secretKeyLabel: 'Secret Key',
      saveKeyBtn: 'Save Encrypted Key',
      paramsTitle: 'Profit & Risk Parameters',
      minProfitLabel: 'Min Target Profit (%)',
      maxLossLabel: 'Max Stop Loss (%)',
      capitalLimitLabel: 'Max Capital Per Trade (USD)',
      maxSpreadLabel: 'Max Slippage Tolerance',
      rebalanceTriggerLabel: 'Rebalance Trigger (Foreign USD Share <= %)',
      rebalanceTargetLabel: 'Rebalance Target (Foreign USD Share %)',
      rebalanceMaxPremLabel: 'Max Premium for Rebalance (%)',
      saveParamsBtn: 'Save Parameters',
      paperTitle: 'Paper Account Seed Configuration',
      paperDesc: 'Reset virtual balances and close all open paper positions.',
      paperUpbitKrwLabel: 'Initial Upbit KRW Seed',
      paperCoinbaseUsdLabel: 'Initial Coinbase USD Seed',
      resetPaperBtn: 'Reset Paper Account Now',
      historyTitle: 'Closed Arbitrage History',
      exportCsvBtn: 'Export CSV',
      colOpenedAt: 'Opened At',
      colClosedAt: 'Closed At',
      colRealizedPnl: 'Realized PnL',
      noHistory: 'No closed trades recorded yet.'
    },
    stopModal: {
      title: 'Active Positions Detected',
      desc: 'How would you like to handle your currently open positions?',
      optDrainTitle: 'Stop New Entries Only (Drain Mode)',
      optDrainDesc: 'Recommended. Existing positions continue to be monitored until target exit.',
      optCloseTitle: 'Close All Positions at Market & Stop Immediately',
      optCloseDesc: 'Immediately unwinds all open positions and stops the bot.',
      cancel: 'Cancel'
    }
  }
};

export const formatStrategyLabel = (rawType, lang = 'ko') => {
  const s = String(rawType || '').toUpperCase();
  if (lang === 'en') {
    if (s.includes('TRANSFER') || s.includes('(C)')) return 'Transfer Arbitrage (Spot → Transfer)';
    if (s.includes('MARGIN_FUTURE') || s.includes('MARGIN-FUTURE') || s.includes('(B)')) return 'Margin Short + Futures Long';
    if (s.includes('FUTURE_SPOT') || s.includes('FUTURE-SPOT') || s.includes('(D)')) return 'Futures Short + Spot Long';
    if (s.includes('FUTURE_FUTURE') || s.includes('FUTURE-FUTURE') || s.includes('(E)')) return 'Futures Short + Futures Long';
    return 'Margin Short + Spot Long';
  }
  if (s.includes('TRANSFER') || s.includes('(C)')) return '김프 전송 차익 (해외매수 → 국내전송)';
  if (s.includes('MARGIN_FUTURE') || s.includes('MARGIN-FUTURE') || s.includes('(B)')) return '마진 숏 + 해외 선물 롱 헤징';
  if (s.includes('FUTURE_SPOT') || s.includes('FUTURE-SPOT') || s.includes('(D)')) return '선물 숏 + 해외 현물 롱 헤징';
  if (s.includes('FUTURE_FUTURE') || s.includes('FUTURE-FUTURE') || s.includes('(E)')) return '양방향 선물 롱·숏 수렴 헤징';
  return '마진 대여 숏 + 해외 현물 롱 헤징';
};

export const formatPositionStatus = (rawStatus, lang = 'ko') => {
  const st = String(rawStatus || 'OPEN').toUpperCase();
  if (lang === 'en') {
    if (st === 'TRANSFERRING') return 'Transferring On-Chain';
    if (st === 'REPAYING' || st === 'AWAITING_REPAY') return 'Awaiting Loan Repay';
    if (st === 'CLOSED') return 'Closed';
    return 'Hedged (Open)';
  }
  if (st === 'TRANSFERRING') return '온체인 코인 전송 중';
  if (st === 'REPAYING' || st === 'AWAITING_REPAY') return '대여 상환 대기';
  if (st === 'CLOSED') return '청산 완료';
  return '델타 중립 헤징 중';
};
