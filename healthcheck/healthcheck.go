package healthcheck

import (
	"context"
	"fmt"
	"io" // Добавлен импорт io
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy" // Убедимся, что импорт есть
	"github.com/go-resty/resty/v2"
)

func init() {
	caddy.RegisterModule(SlackNotifierHealthChecker{})
}

// SlackNotifierHealthChecker реализует reverseproxy.UpstreamHealthChecker
type SlackNotifierHealthChecker struct {
	URI             string         `json:"uri,omitempty"`
	Interval        caddy.Duration `json:"interval,omitempty"`
	Timeout         caddy.Duration `json:"timeout,omitempty"`
	Body            string         `json:"body,omitempty"`
	SlackWebhookURL string         `json:"slack_webhook_url,omitempty"`

	mu             sync.Mutex
	endpointStatuses map[string]bool
	httpClient     *http.Client
}

func (SlackNotifierHealthChecker) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.health_checks.slack_notifier",
		New: func() caddy.Module { return new(SlackNotifierHealthChecker) },
	}
}

func (s *SlackNotifierHealthChecker) Provision(ctx caddy.Context) error {
	s.mu.Lock()
	if s.endpointStatuses == nil {
		s.endpointStatuses = make(map[string]bool)
	}
	s.mu.Unlock()

	if s.Interval == 0 {
		s.Interval = caddy.Duration(30 * time.Second)
	}
	if s.Timeout == 0 {
		s.Timeout = caddy.Duration(10 * time.Second)
	}
    if s.URI == "" {
        return fmt.Errorf("health check URI is required")
    }

	s.httpClient = &http.Client{
		Timeout: time.Duration(s.Timeout),
	}

	log.Printf("[INFO] SlackNotifierHealthChecker Provisioned: URI=%s, Interval=%v, Timeout=%v", s.URI, s.Interval, s.Timeout)
	return nil
}

func (s *SlackNotifierHealthChecker) Check(ctx context.Context, upstream *reverseproxy.Upstream) error {
	if upstream == nil || upstream.Dial == "" {
		return fmt.Errorf("invalid upstream for health check")
	}

	checkURL, err := s.buildCheckURL(upstream)
	if err != nil {
		return fmt.Errorf("building health check URL: %w", err)
	}

	isAvailable, checkErr := s.performCheck(ctx, checkURL)

	upstreamAddr := upstream.Dial

	s.mu.Lock()
	previousStatus, exists := s.endpointStatuses[upstreamAddr]
	if !exists {
		previousStatus = true
	}
	currentStatus := isAvailable && checkErr == nil
	s.endpointStatuses[upstreamAddr] = currentStatus
	s.mu.Unlock()

	if currentStatus != previousStatus {
		var message string
		if currentStatus {
			message = fmt.Sprintf("Upstream %s is back online! (Checked %s)", upstreamAddr, s.URI)
		} else {
			errMsg := "unhealthy"
			if checkErr != nil {
				errMsg = fmt.Sprintf("check error: %v", checkErr)
			}
			message = fmt.Sprintf("Upstream %s is now DOWN! (Checked %s) - Reason: %s", upstreamAddr, s.URI, errMsg)
		}
		log.Println("[INFO]", message)
		s.sendSlackNotification(message)
	} else {
         log.Printf("[DEBUG] SlackNotifierHealthChecker: Upstream %s status unchanged (%v)", upstreamAddr, currentStatus)
    }

	if checkErr != nil {
		return checkErr
	}
	if !isAvailable {
		return fmt.Errorf("upstream reported unhealthy status")
	}

	return nil
}

func (s *SlackNotifierHealthChecker) buildCheckURL(upstream *reverseproxy.Upstream) (string, error) {
	// Используем scheme из upstream, если он есть, иначе предполагаем по порту
	scheme := upstream.Scheme()
	if scheme == "" {
        if strings.HasSuffix(upstream.Dial, ":443") {
            scheme = "https"
        } else {
            scheme = "http"
        }
    }

	// Получаем хост и порт из upstream.Dial
	host, port, err := net.SplitHostPort(upstream.Dial)
	if err != nil {
		host = upstream.Dial
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
        // Проверим, что теперь адрес корректный
        addr := net.JoinHostPort(host, port)
        if _, _, err := net.SplitHostPort(addr); err != nil {
             return "", fmt.Errorf("invalid upstream address format '%s': %w", upstream.Dial, err)
        }
	}


	uriPath := "/" + strings.TrimPrefix(s.URI, "/")
	url := fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, port), uriPath)
	return url, nil
}

func (s *SlackNotifierHealthChecker) performCheck(ctx context.Context, checkURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "Caddy-Health-Checker")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("[DEBUG] SlackNotifierHealthChecker: Request to %s failed: %v", checkURL, err)
		return false, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Проверка статус-кода (2xx считается успехом)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // Исправлено: resp.StatusCode
		log.Printf("[DEBUG] SlackNotifierHealthChecker: Endpoint %s: Status code %d", checkURL, resp.StatusCode) // Исправлено
		return false, nil
	}

	// Проверка тела ответа, если указано
	if s.Body != "" {
		bodyBytes, err := io.ReadAll(resp.Body) // Используем io.ReadAll
		if err != nil {
			log.Printf("[DEBUG] SlackNotifierHealthChecker: Endpoint %s: Failed to read body: %v", checkURL, err)
			return false, fmt.Errorf("reading response body: %w", err)
		}
		if !strings.Contains(string(bodyBytes), s.Body) {
			log.Printf("[DEBUG] SlackNotifierHealthChecker: Endpoint %s: Body mismatch", checkURL)
			return false, nil
		}
	}

	return true, nil
}


func (s *SlackNotifierHealthChecker) sendSlackNotification(message string) {
	slackURL := s.SlackWebhookURL
	if slackURL == "" {
		slackURL = os.Getenv("SLACK_WEBHOOK_URL")
	}

	if slackURL == "" {
		log.Println("[WARN] SlackNotifierHealthChecker: Slack webhook URL not configured, skipping notification.")
		return
	}

	payload := map[string]string{"text": message}
	client := resty.New()
	_, err := client.R().
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		Post(slackURL)
	if err != nil {
		log.Printf("[ERROR] SlackNotifierHealthChecker: Failed to send Slack notification: %v", err)
	} else {
        log.Printf("[INFO] SlackNotifierHealthChecker: Sent Slack notification for: %s", message)
    }
}

func (s *SlackNotifierHealthChecker) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() { // Перебираем директивы внутри блока slack_notifier { ... }
		switch d.Val() {
		case "uri":
			if !d.AllArgs(&s.URI) {
				return d.ArgErr()
			}
		case "interval":
			if !d.NextArg() { return d.ArgErr() }
			dur, err := time.ParseDuration(d.Val())
			if err != nil { return d.Errf("parsing interval duration '%s': %v", d.Val(), err) }
			s.Interval = caddy.Duration(dur)
		case "timeout":
			if !d.NextArg() { return d.ArgErr() }
			dur, err := time.ParseDuration(d.Val())
			if err != nil { return d.Errf("parsing timeout duration '%s': %v", d.Val(), err) }
			s.Timeout = caddy.Duration(dur)
		case "body":
			if !d.AllArgs(&s.Body) {
				return d.ArgErr()
			}
		case "slack_webhook_url":
			if !d.AllArgs(&s.SlackWebhookURL) {
				return d.ArgErr()
			}
		default:
			return d.Errf("unrecognized subdirective '%s'", d.Val())
		}
	}
	return nil
}

var (
	_ caddy.Module                    = (*SlackNotifierHealthChecker)(nil)
	_ caddy.Provisioner               = (*SlackNotifierHealthChecker)(nil)
	_ caddyfile.Unmarshaler           = (*SlackNotifierHealthChecker)(nil)
	_ reverseproxy.UpstreamHealthChecker = (*SlackNotifierHealthChecker)(nil)
)
