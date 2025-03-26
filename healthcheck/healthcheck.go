package healthcheck

import (
	"context"
	"log"

	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(MinimalNotifier{}) // Регистрируем новый тип
	log.Println("[DEBUG] MinimalNotifier Registered in init()")
}

// MinimalNotifier - минимальное приложение Caddy
type MinimalNotifier struct {
    SomeSetting string `json:"some_setting,omitempty"` // Пример настройки
    ctx caddy.Context
}

// CaddyModule возвращает информацию о модуле Caddy.
func (MinimalNotifier) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.apps.health_notifier", // Оставляем тот же ID!
		New: func() caddy.Module { return new(MinimalNotifier) },
	}
}

// Provision - минимальная реализация
func (m *MinimalNotifier) Provision(ctx caddy.Context) error {
	m.ctx = ctx
	log.Printf("[INFO] MinimalNotifier Provisioned. Setting: %s", m.SomeSetting)
	return nil
}

// Start - минимальная реализация
func (m *MinimalNotifier) Start() error {
	log.Println("[INFO] MinimalNotifier App Started.")
	// Запускаем пустую горутину, чтобы приложение не завершалось сразу
	go func() {
        <-m.ctx.Done() // Ждем завершения контекста Caddy
        log.Println("[INFO] MinimalNotifier context done, exiting goroutine.")
    }()
	return nil
}

// Stop - минимальная реализация
func (m *MinimalNotifier) Stop() error {
	log.Println("[INFO] MinimalNotifier App Stopped.")
	return nil
}

// Cleanup - минимальная реализация
func (m *MinimalNotifier) Cleanup() error {
	log.Println("[INFO] MinimalNotifier App Cleaning up.")
    return m.Stop()
}


// Interface guards (только необходимые для App)
var (
	_ caddy.App          = (*MinimalNotifier)(nil)
	_ caddy.Provisioner  = (*MinimalNotifier)(nil)
	_ caddy.CleanerUpper = (*MinimalNotifier)(nil)
)
