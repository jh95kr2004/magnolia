import { describe, it, expect } from 'vitest';
import {
  translations,
  formatStrategyLabel,
  formatPositionStatus,
  formatEventBadge
} from '../i18n/translations';

describe('i18n translations and domain formatters', () => {
  it('contains matching top-level sections for ko and en', () => {
    expect(Object.keys(translations.ko).sort()).toEqual(Object.keys(translations.en).sort());
  });

  it('formats strategy codes into clear human-readable labels in KO and EN', () => {
    expect(formatStrategyLabel('Transfer Loop (C)', 'ko')).toContain('전송 차익');
    expect(formatStrategyLabel('FOREIGN_TRANSFER', 'en')).toContain('Transfer Arbitrage');
    expect(formatStrategyLabel('Margin-Future (B)', 'ko')).toContain('해외 선물 롱');
    expect(formatStrategyLabel('Future-Spot (D)', 'ko')).toContain('선물 숏');
    expect(formatStrategyLabel('Future-Future (E)', 'ko')).toContain('양방향 선물');
    expect(formatStrategyLabel('Margin-Spot (A)', 'ko')).toContain('마진 대여 숏');
  });

  it('formats position status codes including STAGED_ASSASSINATION and PRE_STAGED_LISTING', () => {
    expect(formatPositionStatus('OPEN', 'ko')).toBe('델타 중립 헤징 중');
    expect(formatPositionStatus('TRANSFERRING', 'ko')).toBe('온체인 코인 전송 중');
    expect(formatPositionStatus('STAGED_ASSASSINATION', 'ko')).toContain('암살 대기 중');
    expect(formatPositionStatus('PRE_STAGED_LISTING', 'ko')).toContain('상장 대기 장전 중');
    expect(formatPositionStatus('AWAITING_REPAY', 'ko')).toBe('대여 상환 대기');
    expect(formatPositionStatus('CLOSED', 'en')).toBe('Closed');
  });

  it('formats LLM catalyst event badges in KO and EN', () => {
    expect(formatEventBadge('NEW_KRW_LISTING', 'ko')?.label).toContain('원화 상장 공지');
    expect(formatEventBadge('INVESTMENT_WARNING', 'ko')?.label).toContain('유의 종목 지정');
    expect(formatEventBadge('WALLET_SUSPENSION', 'ko')?.label).toContain('입출금 중단 예고');
    expect(formatEventBadge('DELISTING', 'en')?.label).toContain('Delisting');
  });
});
