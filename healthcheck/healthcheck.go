package healthcheck

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/go-resty/resty/v2"
)

func init() {
	caddy.RegisterModule(&HealthNotifier{}) // Регистрируем как HealthNotifier
}

// HealthNotifier - приложение Caddy для мониторинга и уведомлений.
type HealthNotifier struct {
	Endpoints       []string       `json:"endpoints,omitempty"`
	Interval        caddy.Duration `json:"interval,omitempty"`
	Timeout         caddy.Duration `json:"timeout,omitempty"`
	HealthURI       string         `json:"health_uri,omitempty"`
	HealthBody      string         `json:"health_body,omitempty"`
	SlackWebhookURL string         `json:"slack_webhook_url,omitempty"`

	endpointStatuses map[string]bool // Ключ - полный URL проверки
	mu               sync.Mutex
	stopChan         chan struct{}
	wg               sync.WaitGroup
	ctx              caddy.Context
}

// CaddyModule возвращает информацию о модуле Caddy.
func (*HealthNotifier) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.apps.health_notifier", // Уникальный ID для приложения
		New: func() caddy.Module { return new(HealthNotifier) },
	}
}

// Provision настраивает модуль.
func (hn *HealthNotifier) Provision(ctx caddy.Context) error {
	hn.ctx = ctx
	hn.endpointStatuses = make(map[string]bool)
	hn.stopChan = make(chan struct{})

	// Значения по умолчанию
	if hn.Interval == 0 {
		hn.Interval = caddy.Duration(10 * time.Second)
	}
	if hn.Timeout == 0 {
		hn.Timeout = caddy.Duration(5 * time.Second)
	}
	if hn.HealthURI == "" {
		return fmt.Errorf("health_uri is required for health_notifier app")
	}

	// Инициализация статусов (ключ - полный URL)
	for _, endpointBase := range hn.Endpoints {
		checkURL := strings.TrimSuffix(endpointBase, "/") + "/" + strings.TrimPrefix(hn.HealthURI, "/")
		hn.endpointStatuses[checkURL] = true // Изначально считаем доступными
	}

	log.Printf("[INFO] HealthNotifier Provisioned: Interval=%v, Timeout=%v, URI=%s, Endpoints=%v", hn.Interval, hn.Timeout, hn.HealthURI, hn.Endpoints)
	return nil
}

// Start запускает фоновый мониторинг.
func (hn *HealthNotifier) Start() error {
	hn.wg.Add(1)
	go hn.runChecker()
	log.Println("[INFO] HealthNotifier App Started.")
	return nil
}

// Stop останавливает фоновый мониторинг.
func (hn *HealthNotifier) Stop() error {
	select {
	case <-hn.stopChan:
		// Уже закрыт
	default:
		close(hn.stopChan)
	}
	hn.wg.Wait()
	log.Println("[INFO] HealthNotifier App Stopped.")
	return nil
}

// Cleanup также останавливает мониторинг.
func (hn *HealthNotifier) Cleanup() error {
	return hn.Stop()
}

// runChecker - основная горутина мониторинга
func (hn *HealthNotifier) runChecker() {
	defer hn.wg.Done()
	ticker := time.NewTicker(time.Duration(hn.Interval))
	defer ticker.Stop()

	log.Printf("[INFO] HealthNotifier started checking %d base endpoints every %v", len(hn.Endpoints), hn.Interval)

	// Первоначальная проверка
	hn.performChecks()

	for {
		select {
		case <-ticker.C:
			hn.performChecks()
		case <-hn.stopChan:
			log.Println("[INFO] HealthNotifier stopping checker goroutine (stopChan)...")
			return
		case <-hn.ctx.Done():
			log.Println("[INFO] HealthNotifier stopping checker goroutine (context done)...")
			select {
			case <-hn.stopChan:
			default:
				close(hn.stopChan) // Убедимся, что канал закрыт
			}
			return
		}
	}
}

// performChecks выполняет проверку всех конечных точек
func (hn *HealthNotifier) performChecks() {
	hn.mu.Lock()                                // Блокируем доступ к endpointsStatuses на время итерации
	endpointsToCheck := make(map[string]string) // checkURL -> endpointBase
	for _, base := range hn.Endpoints {
		checkURL := strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(hn.HealthURI, "/")
		endpointsToCheck[checkURL] = base
	}
	hn.mu.Unlock() // Разблокируем после копирования

	for checkURL, endpointBase := range endpointsToCheck {
		hn.checkEndpoint(checkURL, endpointBase)
	}
}

// checkEndpoint проверяет статус одной конечной точки
func (hn *HealthNotifier) checkEndpoint(checkURL string, endpointBase string) {
	isAvailable, err := hn.isEndpointAvailable(checkURL)
	if err != nil {
		log.Printf("[ERROR] HealthNotifier: Error checking endpoint %s: %v", checkURL, err)
		// Решаем, нужно ли считать ошибку как "недоступен" для уведомления
		isAvailable = false // Считаем недоступным при ошибке
	}

	hn.mu.Lock()
	previousStatus, exists := hn.endpointStatuses[checkURL]
	if !exists {
		previousStatus = true // Считаем, что был здоров, если не было записи
	}
	currentStatus := isAvailable // Статус определяется результатом проверки
	hn.endpointStatuses[checkURL] = currentStatus
	hn.mu.Unlock()

	if currentStatus != previousStatus {
		var message string
		if currentStatus {
			message = fmt.Sprintf("HealthNotifier: Endpoint %s (%s) is back online!", endpointBase, hn.HealthURI)
		} else {
			reason := "unhealthy status or body"
			if err != nil {
				reason = fmt.Sprintf("check error: %v", err)
			}
			message = fmt.Sprintf("HealthNotifier: Endpoint %s (%s) is now DOWN! Reason: %s", endpointBase, hn.HealthURI, reason)
		}
		log.Println("[INFO]", message)
		hn.sendSlackNotification(message)
	} else {
		log.Printf("[DEBUG] HealthNotifier: Endpoint %s (%s) status unchanged (%v)", endpointBase, hn.HealthURI, currentStatus)
	}
}

// isEndpointAvailable выполняет HTTP-проверку
func (hn *HealthNotifier) isEndpointAvailable(checkURL string) (bool, error) {
	httpClient := &http.Client{ // Создаем новый клиент для каждого запроса (или используем из hn.httpClient)
		Timeout: time.Duration(hn.Timeout),
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, checkURL, nil) // Используем background context
	if err != nil {
		return false, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "Caddy-Health-Notifier")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[DEBUG] HealthNotifier: Request to %s failed: %v", checkURL, err)
		return false, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			log.Println(err)
		}
	}(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[DEBUG] HealthNotifier: Endpoint %s: Status code %d", checkURL, resp.StatusCode)
		return false, nil // Не ошибка, просто не здоров
	}

	if hn.HealthBody != "" {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[DEBUG] HealthNotifier: Endpoint %s: Failed to read body: %v", checkURL, err)
			return false, fmt.Errorf("reading response body: %w", err)
		}
		if !strings.Contains(string(bodyBytes), hn.HealthBody) {
			log.Printf("[DEBUG] HealthNotifier: Endpoint %s: Body mismatch", checkURL)
			return false, nil // Не ошибка, просто не здоров
		}
	}

	return true, nil
}

// sendSlackNotification отправляет сообщение в Slack
func (hn *HealthNotifier) sendSlackNotification(message string) {
	slackURL := hn.SlackWebhookURL
	if slackURL == "" {
		slackURL = os.Getenv("SLACK_WEBHOOK_URL")
	}
	if slackURL == "" {
		log.Println("[WARN] HealthNotifier: Slack webhook URL not configured, skipping notification.")
		return
	}
	payload := map[string]string{"text": message}
	client := resty.New()
	_, err := client.R().
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		Post(slackURL)
	if err != nil {
		log.Printf("[ERROR] HealthNotifier: Failed to send Slack notification: %v", err)
	} else {
		log.Printf("[INFO] HealthNotifier: Sent Slack notification.")
	}
}

// --- Caddyfile Unmarshaling ---

// UnmarshalCaddyfile настраивает приложение из Caddyfile.
func (hn *HealthNotifier) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Пропускаем имя приложения "health_notifier"
	// d.Next()

	// Ищем открывающую скобку или конец строки
	if !d.NextArg() {
		// Нет аргументов после имени приложения, возможно пустой блок {} или просто имя
		if d.Next() && d.Val() == "{" { // Проверяем наличие {
			// Пустой блок, ничего не делаем
			if d.Next() && d.Val() == "}" {
				return nil // Все нормально, пустой блок
			} else {
				return d.Err("expected '}' after '{'")
			}
		}
		// Если не было '{', значит конфигурации нет, используем умолчания
		return nil
	}
	// Если первый аргумент не '{', то это ошибка синтаксиса
	if d.Val() != "{" {
		return d.Err("expected '{' after app name or nothing")
	}

	for d.NextBlock(0) { // Перебираем директивы внутри блока { ... }
		switch d.Val() {
		case "endpoints":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			hn.Endpoints = append(hn.Endpoints, args...) // Добавляем, а не перезаписываем, если несколько раз указано
		case "interval":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("parsing interval duration '%s': %v", d.Val(), err)
			}
			hn.Interval = caddy.Duration(dur)
		case "timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("parsing timeout duration '%s': %v", d.Val(), err)
			}
			hn.Timeout = caddy.Duration(dur)
		case "health_uri":
			if !d.NextArg() {
				return d.ArgErr()
			}
			hn.HealthURI = d.Val()
		case "health_body":
			if !d.NextArg() {
				return d.ArgErr()
			}
			hn.HealthBody = d.Val()
		case "slack_webhook_url":
			if !d.NextArg() {
				return d.ArgErr()
			}
			hn.SlackWebhookURL = d.Val()
		default:
			return d.Errf("unrecognized health_notifier option: %s", d.Val())
		}
	}
	return nil
}

// Interface guards
var (
	_ caddy.App             = (*HealthNotifier)(nil) // Реализуем интерфейс App
	_ caddy.Provisioner     = (*HealthNotifier)(nil)
	_ caddy.CleanerUpper    = (*HealthNotifier)(nil) // Для Stop/Cleanup
	_ caddyfile.Unmarshaler = (*HealthNotifier)(nil)
)
