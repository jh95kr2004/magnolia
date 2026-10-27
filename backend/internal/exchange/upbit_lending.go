/*
Package exchange
This file implements UpbitLendingDriver (adhering to WebLendingDriver) to automate Upbit's
web-based Coin Lending service ("코인 빌리기") via Chrome DevTools Protocol (CDP) on the noVNC browser.
*/
package exchange

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	upbitWebLoginURL   = "https://upbit.com/service_center/login"
	upbitWebLendingURL = "https://upbit.com/staking/lending"
)

type UpbitLendingDriver struct{}

func NewUpbitLendingDriver() *UpbitLendingDriver {
	return &UpbitLendingDriver{}
}

func (d *UpbitLendingDriver) ExchangeName() string {
	return "UPBIT"
}

func (d *UpbitLendingDriver) LoginURL() string {
	return upbitWebLoginURL
}

func (d *UpbitLendingDriver) LendingURL(currency Currency) string {
	if currency != "" {
		return fmt.Sprintf("%s?code=%s", upbitWebLendingURL, strings.ToUpper(string(currency)))
	}
	return upbitWebLendingURL
}

// CheckLoggedIn inspects the Upbit web session inside the browser tab to determine if the user is authenticated.
func (d *UpbitLendingDriver) CheckLoggedIn(ctx context.Context, cdp *CDPSession) (bool, error) {
	jsCheck := `(function() {
		try {
			const hasAuthToken = document.cookie.includes('access_token') ||
				localStorage.getItem('upbit_logged_in') === 'true' ||
				!!document.querySelector('a[href*="logout"], button[class*="logout"], .Mypage, [data-testid="user-profile"]');
			const isLoginPage = window.location.pathname.includes('/login');
			if (isLoginPage && !hasAuthToken) return 'LOGGED_OUT';
			return hasAuthToken ? 'LOGGED_IN' : 'LOGGED_OUT';
		} catch (e) {
			return 'LOGGED_OUT';
		}
	})()`

	res, err := cdp.EvaluateJS(ctx, upbitWebLendingURL, jsCheck)
	if err != nil {
		return false, err
	}
	return res == "LOGGED_IN", nil
}

// KeepAlive touches the session state on Upbit's lending view so idle session timeouts do not trigger.
func (d *UpbitLendingDriver) KeepAlive(ctx context.Context, cdp *CDPSession) error {
	jsKeepAlive := `(function() {
		window.dispatchEvent(new Event('mousemove'));
		return 'OK';
	})()`
	_, err := cdp.EvaluateJS(ctx, upbitWebLendingURL, jsKeepAlive)
	return err
}

// Borrow navigates to the Upbit Coin Lending page for the target currency, inputs the requested borrow amount,
// accepts lending terms, and submits the borrow order.
func (d *UpbitLendingDriver) Borrow(ctx context.Context, cdp *CDPSession, currency Currency, amount float64) error {
	targetURL := d.LendingURL(currency)
	if err := cdp.Navigate(ctx, targetURL); err != nil {
		return fmt.Errorf("upbit web borrow navigation failed: %w", err)
	}

	// Allow DOM to render lending modal/form
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(800 * time.Millisecond):
	}

	jsBorrow := fmt.Sprintf(`(async function() {
		const targetCoin = %q;
		const borrowAmt = %f;

		// 1. Locate coin lending row or input field
		const input = document.querySelector('input[data-lending-input="borrow"], input[placeholder*="대여"], input[placeholder*="수량"], input[type="number"], input[type="text"].lending-amount');
		if (!input) {
			return 'ERR_BORROW_INPUT_NOT_FOUND:' + targetCoin;
		}

		const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		nativeSetter.call(input, borrowAmt.toFixed(8));
		input.dispatchEvent(new Event('input', { bubbles: true }));
		input.dispatchEvent(new Event('change', { bubbles: true }));

		// 2. Check agreement checkbox if present
		const checkbox = document.querySelector('input[type="checkbox"]:not(:checked)');
		if (checkbox) {
			checkbox.click();
		}

		// 3. Click Borrow submit button
		const buttons = Array.from(document.querySelectorAll('button, a[role="button"]'));
		const submitBtn = buttons.find(b => b.innerText && (b.innerText.includes('빌리기') || b.innerText.includes('대여 신청') || b.innerText.includes('대여하기')));
		if (!submitBtn) {
			return 'ERR_BORROW_SUBMIT_BTN_NOT_FOUND';
		}
		submitBtn.click();

		// 4. Confirm modal dialog if popped up
		await new Promise(r => setTimeout(r, 400));
		const confirmBtns = Array.from(document.querySelectorAll('button'));
		const confirmBtn = confirmBtns.find(b => b.innerText && (b.innerText.trim() === '확인' || b.innerText.includes('신청 확인')));
		if (confirmBtn) {
			confirmBtn.click();
		}
		return 'BORROW_SUBMITTED';
	})()`, string(currency), amount)

	res, err := cdp.EvaluateJS(ctx, targetURL, jsBorrow)
	if err != nil {
		return fmt.Errorf("upbit web borrow JS execution failed: %w", err)
	}
	if strings.HasPrefix(res, "ERR_") {
		return fmt.Errorf("upbit web borrow rejected by DOM: %s", res)
	}
	return nil
}

// Repay navigates to Upbit's active lending history and executes repayment for the specified currency and amount.
func (d *UpbitLendingDriver) Repay(ctx context.Context, cdp *CDPSession, currency Currency, amount float64) error {
	targetURL := d.LendingURL(currency)
	if err := cdp.Navigate(ctx, targetURL); err != nil {
		return fmt.Errorf("upbit web repay navigation failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(800 * time.Millisecond):
	}

	jsRepay := fmt.Sprintf(`(async function() {
		const targetCoin = %q;
		const repayAmt = %f;

		const buttons = Array.from(document.querySelectorAll('button, a[role="button"]'));
		const repayTabOrBtn = buttons.find(b => b.innerText && (b.innerText.includes('상환') || b.innerText.includes('갚기')));
		if (!repayTabOrBtn) {
			return 'ERR_REPAY_BTN_NOT_FOUND:' + targetCoin;
		}
		repayTabOrBtn.click();

		await new Promise(r => setTimeout(r, 300));
		const input = document.querySelector('input[data-lending-input="repay"], input[placeholder*="상환"], input[placeholder*="수량"], input[type="number"]');
		if (input) {
			const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
			nativeSetter.call(input, repayAmt.toFixed(8));
			input.dispatchEvent(new Event('input', { bubbles: true }));
			input.dispatchEvent(new Event('change', { bubbles: true }));
		}

		const confirmBtns = Array.from(document.querySelectorAll('button'));
		const submitBtn = confirmBtns.find(b => b.innerText && (b.innerText.includes('상환 신청') || b.innerText.includes('상환하기') || b.innerText.trim() === '확인'));
		if (!submitBtn) {
			return 'ERR_REPAY_CONFIRM_NOT_FOUND';
		}
		submitBtn.click();
		return 'REPAY_SUBMITTED';
	})()`, string(currency), amount)

	res, err := cdp.EvaluateJS(ctx, targetURL, jsRepay)
	if err != nil {
		return fmt.Errorf("upbit web repay JS execution failed: %w", err)
	}
	if strings.HasPrefix(res, "ERR_") {
		return fmt.Errorf("upbit web repay rejected by DOM: %s", res)
	}
	return nil
}
