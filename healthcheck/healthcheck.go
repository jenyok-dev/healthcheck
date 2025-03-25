package healthcheck

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/go-resty/resty/v2"
)

// Interface guards
var (
	_ caddy.Module                       = (*HealthCheck)(nil)
	_ caddy.Provisioner                  = (*HealthCheck)(nil)
	_ reverseproxy.ConfigChecker          = (*HealthCheck)(nil) // реализуем интерфейс ConfigChecker
	_ caddyfile.Unmarshaler                = (*HealthCheck)(nil)
)

// HealthCheck представляет конфигурацию модуля healthcheck.
type HealthCheck struct {
	Upstream        string `json:"upstream"`
	HealthURI       string `json:"health_uri"`
	Interval        string `json:"interval"`
	Timeout         string `json:"timeout"`
	HealthBody      string `json:"health_body"`
    	SlackWebhookURL string `json:"slack_webhook_url"`
	EndpointStatuses map[string]bool    `json:"-"`
	intervalDuration time.Duration
	timeoutDuration  time.Duration
	mu             sync.Mutex
	transport      http.RoundTripper
}

// CaddyModule returns module information.
func (HealthCheck) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "http.reverse_proxy.health_check",
		New: func() caddy.Module { return new(HealthCheck) },
	}
}

// Provision sets up the health check module.
func (h *HealthCheck) Provision(ctx caddy.Context) error {
	var err error
	h.intervalDuration, err = time.ParseDuration(h.Interval)
	if err != nil {
		return fmt.Errorf("invalid interval: %w", err)
	}
	h.timeoutDuration, err = time.ParseDuration(h.Timeout)
	if err != nil {
		return fmt.Errorf("invalid timeout: %w", err)
	}

    h.EndpointStatuses = make(map[string]bool)
	h.EndpointStatuses[h.Upstream] = true // Изначально считаем все доступными

	go h.startHealthChecks()

	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (h *HealthCheck) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "uri":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.HealthURI = d.Val()
			case "interval":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Interval = d.Val()
			case "timeout":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Timeout = d.Val()
			case "body":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.HealthBody = d.Val()
			case "to":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Upstream = d.Val()
            case "slack_webhook_url": // Добавляем обработку slack_webhook_url
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.SlackWebhookURL = d.Val()
			default:
				return d.Errf("unknown option '%s'", d.Val())
			}
		}
	}
	return nil
}

// CheckConfig implements reverseproxy.ConfigChecker.  It is called during
// configuration to validate the health check config.
func (h *HealthCheck) CheckConfig(hc *reverseproxy.Handler) error {
	// This method is intentionally left blank.  We don't need to do any
	// special configuration checks beyond what's done in Provision and
	// UnmarshalCaddyfile.
	return nil
}

// startHealthChecks запускает периодические проверки доступности endpoints.
func (h *HealthCheck) startHealthChecks() {
	ticker := time.NewTicker(h.intervalDuration)
	defer ticker.Stop()

	for range ticker.C {
		h.checkEndpoint(h.Upstream)
	}
}

// checkEndpoint проверяет доступность endpoint и логирует, если статус изменился.
func (h *HealthCheck) checkEndpoint(upstream string) {
	isAvailable, err := h.isEndpointAvailable(upstream)
	if err != nil {
		log.Printf("[ERROR] Error checking endpoint %s: %v", upstream, err)
		return
	}

	h.mu.Lock()
	previousStatus := h.EndpointStatuses[upstream]
	h.mu.Unlock()

	if isAvailable != previousStatus {
		h.mu.Lock()
		h.EndpointStatuses[upstream] = isAvailable
		h.mu.Unlock()

		var message string
		if isAvailable {
			message = fmt.Sprintf("Upstream %s is back online!", upstream)
		} else {
			message = fmt.Sprintf("Upstream %s is now DOWN!", upstream)
		}

		log.Println(message)

		// Отправляем уведомление в Slack, если URL настроен
		if h.SlackWebhookURL != "" {
			if err := h.sendSlackNotification(message); err != nil {
				log.Printf("[ERROR] Failed to send Slack notification: %v", err)
			}
		}
	}
}

// isEndpointAvailable выполняет HTTP GET запрос к endpoint и возвращает true, если статус код 200-399 и тело соответствует health_body.
func (h *HealthCheck) isEndpointAvailable(upstream string) (bool, error) {
	client := resty.New()
	client.SetTimeout(h.timeoutDuration)

	endpoint := upstream + h.HealthURI

	resp, err := client.R().Get(endpoint)
	if err != nil {
		return false, err
	}

	if resp.StatusCode() >= 200 && resp.StatusCode() < 400 {
		if h.HealthBody != "" && strings.Contains(string(resp.Body()), h.HealthBody) {
			return true, nil
		} else if h.HealthBody == "" { // if health_body is not configured, just check status code
			return true, nil
		}

		log.Printf("[DEBUG] Endpoint %s: Body does not match health_body", endpoint)
		return false, nil

	}

	log.Printf("[DEBUG] Endpoint %s: Status code is %d", endpoint, resp.StatusCode())
	return false, nil
}

// sendSlackNotification отправляет сообщение в Slack через webhook.
func (h *HealthCheck) sendSlackNotification(message string) error {
	payload := map[string]string{"text": message}

	client := resty.New()
	_, err := client.R().
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		Post(h.SlackWebhookURL)

	return err
}

func init() {
	caddy.RegisterModule(HealthCheck{})
}
