/*
Package exchange
This file defines the extensible WebLendingDriver interface and CDP (Chrome DevTools Protocol) session watchdog
for exchanges that provide coin lending/borrowing via web UI rather than official OpenAPI (e.g., Upbit, Bithumb).
Each user's engine goroutine (runUserEngine) runs a child watchdog goroutine bound to the user's context.
*/
package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// CDPSession provides a lightweight Chrome DevTools Protocol client over WebSocket
// to inspect and automate a headful/headless Chromium instance exposed via noVNC.
type CDPSession struct {
	cdpBaseURL string
	httpClient *http.Client
	mu         sync.Mutex
	cmdID      int64
}

func NewCDPSession(cdpBaseURL string) *CDPSession {
	if cdpBaseURL == "" {
		cdpBaseURL = "http://localhost:9222"
	}
	return &CDPSession{
		cdpBaseURL: cdpBaseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// getOrCreatePageTarget finds an existing page tab or opens a new tab in the noVNC browser.
func (c *CDPSession) getOrCreatePageTarget(ctx context.Context, targetURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.cdpBaseURL+"/json/list", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to reach browser CDP at %s: %w", c.cdpBaseURL, err)
	}
	defer resp.Body.Close()

	var targets []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", fmt.Errorf("failed to decode CDP targets: %w", err)
	}

	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, nil
		}
	}

	// Open a new tab if none exists
	newURL := c.cdpBaseURL + "/json/new?" + url.QueryEscape(targetURL)
	newReq, err := http.NewRequestWithContext(ctx, "PUT", newURL, nil)
	if err != nil {
		return "", err
	}
	newResp, err := c.httpClient.Do(newReq)
	if err != nil {
		return "", fmt.Errorf("failed to create new CDP tab: %w", err)
	}
	defer newResp.Body.Close()

	var created cdpTarget
	if err := json.NewDecoder(newResp.Body).Decode(&created); err != nil {
		return "", err
	}
	return created.WebSocketDebuggerURL, nil
}

// EvaluateJS executes a JavaScript expression inside the active browser tab and returns the string result.
func (c *CDPSession) EvaluateJS(ctx context.Context, targetURL string, jsExpression string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	wsURL, err := c.getOrCreatePageTarget(ctx, targetURL)
	if err != nil {
		return "", err
	}

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to connect to CDP websocket: %w", err)
	}
	defer conn.Close()

	id := atomic.AddInt64(&c.cmdID, 1)
	payload := map[string]interface{}{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression":    jsExpression,
			"returnByValue": true,
			"awaitPromise":  true,
		},
	}

	if err := conn.WriteJSON(payload); err != nil {
		return "", err
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var reply struct {
			ID     int64 `json:"id"`
			Result struct {
				Result struct {
					Type  string      `json:"type"`
					Value interface{} `json:"value"`
				} `json:"result"`
				ExceptionDetails interface{} `json:"exceptionDetails"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := conn.ReadJSON(&reply); err != nil {
			return "", err
		}
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return "", fmt.Errorf("CDP error: %s", reply.Error.Message)
		}
		if reply.Result.ExceptionDetails != nil {
			return "", fmt.Errorf("JS exception during evaluation: %v", reply.Result.ExceptionDetails)
		}
		if reply.Result.Result.Value == nil {
			return "", nil
		}
		return fmt.Sprintf("%v", reply.Result.Result.Value), nil
	}
}

// Navigate directs the browser tab to the specified URL.
func (c *CDPSession) Navigate(ctx context.Context, targetURL string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	wsURL, err := c.getOrCreatePageTarget(ctx, targetURL)
	if err != nil {
		return err
	}

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	id := atomic.AddInt64(&c.cmdID, 1)
	payload := map[string]interface{}{
		"id":     id,
		"method": "Page.navigate",
		"params": map[string]interface{}{
			"url": targetURL,
		},
	}
	return conn.WriteJSON(payload)
}

// WebLendingDriver abstracts exchange-specific DOM selectors and web flows for coin borrowing/repaying.
// Adding a new exchange (e.g., Bithumb) only requires implementing this interface.
type WebLendingDriver interface {
	ExchangeName() string
	LoginURL() string
	LendingURL(currency Currency) string
	CheckLoggedIn(ctx context.Context, cdp *CDPSession) (bool, error)
	KeepAlive(ctx context.Context, cdp *CDPSession) error
	Borrow(ctx context.Context, cdp *CDPSession, currency Currency, amount float64) error
	Repay(ctx context.Context, cdp *CDPSession, currency Currency, amount float64) error
}

// SessionCallbacks allows the per-user strategy Engine to react immediately to web session state changes.
type SessionCallbacks struct {
	OnSessionLost     func(exchangeName string)
	OnSessionRestored func(exchangeName string)
}

// WebLendingManager manages the lifecycle, periodic Keep-Alive, and session health state for a WebLendingDriver.
type WebLendingManager struct {
	driver        WebLendingDriver
	cdp           *CDPSession
	sessionValid  atomic.Bool
	checkInterval time.Duration

	cbMu      sync.RWMutex
	callbacks SessionCallbacks
}

func NewWebLendingManager(driver WebLendingDriver, cdpURL string, checkInterval time.Duration) *WebLendingManager {
	if checkInterval <= 0 {
		checkInterval = 30 * time.Second
	}
	return &WebLendingManager{
		driver:        driver,
		cdp:           NewCDPSession(cdpURL),
		checkInterval: checkInterval,
	}
}

func (m *WebLendingManager) SetCallbacks(cb SessionCallbacks) {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.callbacks = cb
}

// IsSessionValid returns true only if the browser session is currently authenticated and healthy.
func (m *WebLendingManager) IsSessionValid() bool {
	return m.sessionValid.Load()
}

// SetSessionValidForTest allows unit tests or simulated environments to toggle session state.
func (m *WebLendingManager) SetSessionValidForTest(valid bool) {
	m.sessionValid.Store(valid)
}

// OpenLoginPage navigates the noVNC browser to the exchange login page so the user can authenticate via QR/2FA.
func (m *WebLendingManager) OpenLoginPage(ctx context.Context) error {
	return m.cdp.Navigate(ctx, m.driver.LoginURL())
}

// StartWatchdog runs inside the per-user goroutine (runUserEngine) and continuously maintains and verifies
// the browser lending session. When ctx is canceled (e.g., pod draining), the watchdog exits cleanly.
func (m *WebLendingManager) StartWatchdog(ctx context.Context, userID uint) {
	log.Printf("[WebLending-%s] Starting session watchdog for User %d (interval: %s)",
		m.driver.ExchangeName(), userID, m.checkInterval)

	// Perform initial check
	m.verifyAndKeepAlive(ctx, userID, true)

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[WebLending-%s] Stopping session watchdog for User %d", m.driver.ExchangeName(), userID)
			return
		case <-ticker.C:
			m.verifyAndKeepAlive(ctx, userID, false)
		}
	}
}

func (m *WebLendingManager) verifyAndKeepAlive(ctx context.Context, userID uint, isInitial bool) {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	wasValid := m.sessionValid.Load()
	loggedIn, err := m.driver.CheckLoggedIn(checkCtx, m.cdp)
	if err != nil || !loggedIn {
		m.sessionValid.Store(false)
		if wasValid || isInitial {
			log.Printf("⚠️ [WebLending-%s] User %d web session is DISCONNECTED/EXPIRED (err: %v). Blocking margin borrow strategies.",
				m.driver.ExchangeName(), userID, err)
			m.cbMu.RLock()
			onLost := m.callbacks.OnSessionLost
			m.cbMu.RUnlock()
			if onLost != nil {
				onLost(m.driver.ExchangeName())
			}
		}
		return
	}

	// Session is valid; trigger KeepAlive touch to prevent idle expiration
	_ = m.driver.KeepAlive(checkCtx, m.cdp)
	m.sessionValid.Store(true)

	if !wasValid {
		log.Printf("✅ [WebLending-%s] User %d web session is ACTIVE. Margin borrow/repay enabled.",
			m.driver.ExchangeName(), userID)
		m.cbMu.RLock()
		onRestored := m.callbacks.OnSessionRestored
		m.cbMu.RUnlock()
		if onRestored != nil {
			onRestored(m.driver.ExchangeName())
		}
	}
}

func (m *WebLendingManager) Borrow(ctx context.Context, currency Currency, amount float64) error {
	// Pre-flight live session verification before executing borrow
	loggedIn, err := m.driver.CheckLoggedIn(ctx, m.cdp)
	if err != nil || !loggedIn {
		m.sessionValid.Store(false)
		m.cbMu.RLock()
		onLost := m.callbacks.OnSessionLost
		m.cbMu.RUnlock()
		if onLost != nil {
			onLost(m.driver.ExchangeName())
		}
		return fmt.Errorf("%s web lending session expired or unavailable; borrow blocked", m.driver.ExchangeName())
	}

	return m.driver.Borrow(ctx, m.cdp, currency, amount)
}

func (m *WebLendingManager) Repay(ctx context.Context, currency Currency, amount float64) error {
	loggedIn, err := m.driver.CheckLoggedIn(ctx, m.cdp)
	if err != nil || !loggedIn {
		m.sessionValid.Store(false)
		m.cbMu.RLock()
		onLost := m.callbacks.OnSessionLost
		m.cbMu.RUnlock()
		if onLost != nil {
			onLost(m.driver.ExchangeName())
		}
		return fmt.Errorf("%s web lending session expired during repay (ERR_SESSION_EXPIRED)", m.driver.ExchangeName())
	}

	return m.driver.Repay(ctx, m.cdp, currency, amount)
}
