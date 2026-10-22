// Package config загружает настройки приложения из переменных окружения (.env).
package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config — все настраиваемые параметры платформы. Пути, имена контейнеров
// и диапазоны портов вынесены в переменные окружения (изоляция окружения).
type Config struct {
	// PostgreSQL
	DatabaseURL string // DATABASE_URL, например postgres://user:pass@localhost:5432/rathole?sslmode=disable

	// HTTP API
	APIAddr   string // API_ADDR, например :8080
	JWTSecret string // JWT_SECRET
	JWTTTLHrs int    // JWT_TTL_HOURS (по умолчанию 24)

	// RatHole control plane
	RatholeConfigPath string // RATHOLE_CONFIG_PATH, например /etc/rathole/server.toml
	RatholeContainer  string // RATHOLE_CONTAINER, имя docker-контейнера RatHole
	RatholeBindAddr   string // RATHOLE_BIND_ADDR, адрес/порт для подключений клиентов (0.0.0.0:2333)
	RatholePID        int    // RATHOLE_PID — если задан, SIGHUP отправляется напрямую процессу (без Docker)
	RatholePIDFile    string // RATHOLE_PIDFILE — путь к PID-файлу; PID читается из файла при каждой синхронизации
	PortRangeStart    int    // PORT_RANGE_START (включительно)
	PortRangeEnd      int    // PORT_RANGE_END (включительно)
	SystemPortCheck   bool   // SYSTEM_PORT_CHECK=true — проверять занятость порта bind'ом перед выдачей
}

// Load читает .env (если файл существует) и переменные окружения.
func Load() *Config {
	// Игнорируем ошибку: .env опционален (в production переменные приходят из окружения).
	_ = godotenv.Load()
	// ENV_FILE позволяет указать абсолютный путь к env-файлу (например, /opt/publicnat/env.sh
	// в systemd-юните) — удобно, когда WorkingDirectory отличается от расположения .env.
	if ef := os.Getenv("ENV_FILE"); ef != "" {
		_ = godotenv.Load(ef)
	}

	cfg := &Config{
		DatabaseURL:       envStr("DATABASE_URL", "postgres://rathole:rathole@localhost:5543/rathole?sslmode=disable"),
		APIAddr:           envStr("API_ADDR", ":8080"),
		JWTSecret:         envStr("JWT_SECRET", ""),
		JWTTTLHrs:         envInt("JWT_TTL_HOURS", 24),
		RatholeConfigPath: envStr("RATHOLE_CONFIG_PATH", "/etc/rathole/server.toml"),
		RatholeContainer:  envStr("RATHOLE_CONTAINER", "rathole-server"),
		RatholeBindAddr:   envStr("RATHOLE_BIND_ADDR", "0.0.0.0:2333"),
		RatholePID:        envInt("RATHOLE_PID", 0),
		RatholePIDFile:    envStr("RATHOLE_PIDFILE", ""),
		PortRangeStart:    envInt("PORT_RANGE_START", 5000),
		PortRangeEnd:      envInt("PORT_RANGE_END", 6000),
		SystemPortCheck:   envBool("SYSTEM_PORT_CHECK", true),
	}

	if cfg.JWTSecret == "" {
		log.Fatal("config: JWT_SECRET must be set (see .env.example)")
	}
	if cfg.PortRangeStart < 1 || cfg.PortRangeEnd > 65535 || cfg.PortRangeStart > cfg.PortRangeEnd {
		log.Fatalf("config: invalid port range %d-%d", cfg.PortRangeStart, cfg.PortRangeEnd)
	}
	return cfg
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
		log.Printf("WARN config: invalid int for %s=%q, using default %d", key, v, def)
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}
