// Package rathole — потокобезопасная генерация server.toml и перезагрузка RatHole (SIGHUP).
package rathole

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/example/nat-platform/backend/internal/storage"
)

// nameRe — допустимые символы имени туннеля: латиница, цифры, дефис.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)

// subdomainRe — допустимые символы поддомена (RFC-1035 метка).
var subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Generator читает туннели из БД (интерфейс — для тестируемости).
type Generator interface {
	ListAllTunnels(ctx context.Context) ([]storage.Tunnel, error)
}

// Syncer — потокобезопасный синхронизатор БД → server.toml → SIGHUP.
type Syncer struct {
	mu sync.Mutex // предотвращает race condition при параллельных запросах разных пользователей

	gen         Generator
	logger      *slog.Logger
	configPath  string // путь к рабочему server.toml
	bindAddr    string // глобальный [server] bind_addr (порт клиентов RatHole)
	container   string // имя docker-контейнера RatHole
	pid         int    // если > 0 — слать сигнал напрямую процессу, минуя Docker
	pidFile     string // если задан — PID перечитывается из файла перед каждой отправкой сигнала
	dockerBin   string // путь к бинарнику docker (по умолчанию "docker")
	sighupRetry int    // повторы при временных ошибках docker CLI
}

// Options — параметры конструктора Syncer.
type Options struct {
	ConfigPath  string
	BindAddr    string
	Container   string
	PID         int
	PIDFile     string
	DockerBin   string
	Logger      *slog.Logger
	SighupRetry int
}

// NewSyncer создаёт синхронизатор конфигурации.
func NewSyncer(gen Generator, opts Options) *Syncer {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dockerBin := opts.DockerBin
	if dockerBin == "" {
		dockerBin = "docker"
	}
	retry := opts.SighupRetry
	if retry <= 0 {
		retry = 3
	}
	return &Syncer{
		gen:         gen,
		logger:      logger,
		configPath:  opts.ConfigPath,
		bindAddr:    opts.BindAddr,
		container:   opts.Container,
		pid:         opts.PID,
		pidFile:     opts.PIDFile,
		dockerBin:   dockerBin,
		sighupRetry: retry,
	}
}

// SyncRatholeConfig — критический узел системы:
//  1. блокирует мьютекс (последовательная сериализация всех обновлений конфига),
//  2. читает активные туннели из БД,
//  3. генерирует TOML,
//  4. атомарно записывает файл (tmp + rename),
//  5. отправляет SIGHUP процессу/контейнеру RatHole.
func (s *Syncer) SyncRatholeConfig(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := time.Now()
	s.logger.Info("rathole sync started", "config_path", s.configPath)

	tunnels, err := s.gen.ListAllTunnels(ctx)
	if err != nil {
		s.logger.Error("rathole sync failed: database read", "error", err)
		return fmt.Errorf("rathole: list tunnels: %w", err)
	}

	content, err := RenderServerConfig(s.bindAddr, tunnels)
	if err != nil {
		s.logger.Error("rathole sync failed: toml render", "error", err, "tunnels", len(tunnels))
		return err
	}

	if err := s.atomicWrite(content); err != nil {
		s.logger.Error("rathole sync failed: file write", "error", err, "config_path", s.configPath)
		return err
	}
	s.logger.Info("rathole config written atomically",
		"config_path", s.configPath, "services", len(tunnels), "bytes", len(content))

	if err := s.sendSIGHUP(ctx); err != nil {
		s.logger.Error("rathole reload failed: SIGHUP", "error", err,
			"container", s.container, "pid", s.pid)
		return err
	}

	s.logger.Info("rathole config applied successfully",
		"duration_ms", time.Since(start).Milliseconds(), "services", len(tunnels))
	return nil
}

// atomicWrite пишет content в <path>.tmp с fsync и переименовывает поверх рабочего файла.
// rename() атомарен в POSIX — RatHole никогда не прочитает наполовину записанный конфиг.
func (s *Syncer) atomicWrite(content string) error {
	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("rathole: mkdir %s: %w", dir, err)
	}
	tmp := s.configPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("rathole: open temp file: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("rathole: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil { // гарантируем сброс на диск до rename
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("rathole: fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rathole: close temp file: %w", err)
	}
	if err := os.Rename(tmp, s.configPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rathole: atomic rename: %w", err)
	}
	// fsync директории — чтобы сам rename пережил падение машины
	dh, err := os.Open(dir)
	if err == nil {
		_ = dh.Sync()
		_ = dh.Close()
	}
	return nil
}

// sendSIGHUP отправляет сигнал перезагрузки:
//   - через syscall.Kill, если задан RATHOLE_PID или RATHOLE_PIDFILE (RatHole на той же машине);
//   - иначе через `docker kill -s SIGHUP <container>` (RatHole в контейнере).
func (s *Syncer) sendSIGHUP(ctx context.Context) error {
	pid := s.pid
	if pid <= 0 && s.pidFile != "" {
		p, err := readPIDFile(s.pidFile)
		if err != nil {
			return fmt.Errorf("rathole: read pidfile %s: %w", s.pidFile, err)
		}
		pid = p
	}
	if pid > 0 {
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			return fmt.Errorf("rathole: kill -SIGHUP pid=%d: %w", pid, err)
		}
		s.logger.Info("SIGHUP sent to process", "pid", pid, "pidfile", s.pidFile)
		return nil
	}

	if s.container == "" {
		return fmt.Errorf("rathole: neither RATHOLE_PID/RATHOLE_PIDFILE nor RATHOLE_CONTAINER is configured")
	}

	var lastErr error
	for attempt := 1; attempt <= s.sighupRetry; attempt++ {
		cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		// docker kill -s SIGHUP <container> — штатный способ послать сигнал контейнеру
		out, err := execCommandContext(cmdCtx, s.dockerBin, "kill", "-s", "SIGHUP", s.container).CombinedOutput()
		cancel()
		if err == nil {
			s.logger.Info("SIGHUP sent to container", "container", s.container, "attempt", attempt)
			return nil
		}
		lastErr = fmt.Errorf("docker kill -s SIGHUP %s: %w (%s)",
			s.container, err, strings.TrimSpace(string(out)))
		s.logger.Warn("SIGHUP delivery failed, retrying",
			"container", s.container, "attempt", attempt, "error", lastErr)
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}
	return lastErr
}

// ---------- TOML generation ----------

// serviceKey строит имя секции [server.services.<key>] — безопасное, уникальное.
// Используется "user-name-PortXXXX": читаемо в логах RatHole и гарантированно уникально
// благодаря unique(user_id, name) + unique(server_port) в схеме БД.
func serviceKey(t storage.Tunnel) string {
	short := t.ID
	if len(short) >= 8 {
		short = short[:8]
	}
	return sanitizeKey(t.Name) + "-" + short + "-p" + strconv.Itoa(t.ServerPort)
}

var keySanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeKey(in string) string {
	return keySanitizer.ReplaceAllString(in, "_")
}

// escapeTOMLString экранирует значение для помещения в двойные кавычки TOML.
func escapeTOMLString(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, "\r", `\r`)
	v = strings.ReplaceAll(v, "\t", `\t`)
	return v
}

// RenderServerConfig формирует полный текст server.toml из списка туннелей.
// Чистая функция без side effects — легко тестировать.
func RenderServerConfig(bindAddr string, tunnels []storage.Tunnel) (string, error) {
	var b strings.Builder
	b.WriteString("# Auto-generated by Rathole Control Plane. DO NOT EDIT MANUALLY.\n")
	fmt.Fprintf(&b, "# Generated at: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	b.WriteString("[server]\n")
	fmt.Fprintf(&b, "bind_addr = %q\n", bindAddr) // порт для подключения клиентов RatHole
	b.WriteString("\n")

	seen := make(map[string]bool, len(tunnels))
	for _, t := range tunnels {
		key := serviceKey(t)
		if seen[key] {
			return "", fmt.Errorf("rathole: duplicate service key %q", key)
		}
		seen[key] = true

		typ := strings.ToLower(t.Type)
		if typ != "tcp" && typ != "udp" {
			return "", fmt.Errorf("rathole: tunnel %s has invalid type %q", t.ID, t.Type)
		}
		fmt.Fprintf(&b, "[server.services.%s]\n", key)
		fmt.Fprintf(&b, "type = %q\n", typ)
		fmt.Fprintf(&b, "token = \"%s\"\n", escapeTOMLString(t.TunnelToken))
		fmt.Fprintf(&b, "bind_addr = \"0.0.0.0:%d\"\n\n", t.ServerPort)
	}
	return b.String(), nil
}

// ---------- Validation & helpers ----------

// ValidateTunnelName проверяет формат имени туннеля (латиница, цифры, дефис).
func ValidateTunnelName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid tunnel name %q: allowed latin letters, digits and hyphens", name)
	}
	return nil
}

// ValidateSubdomain проверяет формат поддомена.
func ValidateSubdomain(sub string) error {
	if !subdomainRe.MatchString(sub) {
		return fmt.Errorf("invalid subdomain %q: RFC-1035 label required", sub)
	}
	return nil
}

// GenerateToken создаёт криптографически стойкий токен длиной length символов
// (base64-url без padding; default 32 chars => 24 bytes entropy).
func GenerateToken(length int) (string, error) {
	if length < 32 {
		length = 32 // требование: длина от 32 символов
	}
	rawLen := (length*3 + 3) / 4 // достаточно байт для base64 нужной длины
	buf := make([]byte, rawLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rathole: generate token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)[:length]
	return tok, nil
}

// readPIDFile читает и парсит PID из файла (fmt.Sprintf("%d\n", pid) в rathole-wrapper).
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid in %q", path)
	}
	return pid, nil
}
