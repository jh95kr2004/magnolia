/*
Package notification
This file is an asynchronous Notification Manager (Notifier) that sends critical arbitrage executions, error events, and liquidation warnings to external notification channels such as Slack, Telegram, or Discord.
It handles telemetry or admin push messages while the system autonomously executes trades.
*/
package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type AlertType string

const (
	AlertPositionOpened AlertType = "POSITION_OPENED"
	AlertPositionClosed AlertType = "POSITION_CLOSED"
	AlertLiquidation    AlertType = "LIQUIDATION_WARNING"
	AlertTransferFailed AlertType = "TRANSFER_FAILED"
	AlertSystemError    AlertType = "SYSTEM_ERROR"
)

type Notifier struct {
	DiscordWebhookURL  string
	TelegramWebhookURL string // For custom telegram bot API
	httpClient         *http.Client
}

func NewNotifier(discordURL string, telegramURL string) *Notifier {
	return &Notifier{
		DiscordWebhookURL:  discordURL,
		TelegramWebhookURL: telegramURL,
		httpClient:         &http.Client{Timeout: 5 * time.Second},
	}
}

// SendAlert pushes a critical alert to the designated chat platforms
func (n *Notifier) SendAlert(ctx context.Context, alertType AlertType, message string) error {
	formattedMsg := fmt.Sprintf("[%s] Magnolia Arb Bot: %s", alertType, message)

	if n.DiscordWebhookURL != "" {
		go n.sendDiscord(formattedMsg)
	}
	if n.TelegramWebhookURL != "" {
		go n.sendTelegram(formattedMsg)
	}

	return nil
}

func (n *Notifier) sendDiscord(content string) {
	payload := map[string]string{
		"content": content,
		"username": "Magnolia Bot",
	}
	jsonData, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", n.DiscordWebhookURL, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (n *Notifier) sendTelegram(content string) {
	// Telegram specific API logic if needed
	// Example: POST https://api.telegram.org/bot<token>/sendMessage
}
