// Package config реализует хранение локального состояния клиента pnat:
//   - ~/.pnat/config.json  — JWT, метаданные туннелей (server_port, tunnel_token, local_addr);
//   - ~/.pnat/client.toml  — единый конфиг для демона rathole (мультиплексирование всех туннелей);
//   - ~/.pnat/daemon.pid   — PID запущенного демона.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// TunnelType — допустимый протокол туннеля (совпадает с валидацией бэкенда).
type TunnelType string

const (
	TypeTCP TunnelType = "tcp"
	TypeUDP TunnelType = "udp"
)

func (t TunnelType) Valid() bool { return t == TypeTCP || t == TypeUDP }

// Tunnel — локальные метаданные одного туннеля пользователя.
type Tunnel struct {
	Name       string     `json:"name"`
	Type       TunnelType `json:"type"`
	ServerPort int        `json:"server_port"`
	Token      string     `json:"token"`
	LocalAddr  string     `json:"local_addr,omitempty"` // 127.0.0.1:<порт> на машине пользователя
}

// State — содержимое ~/.pnat/config.json.
type State struct {
	ServerURL    string            `json:"server_url"`              // базовый URL REST API
	RemoteAddr   string            `json:"remote_addr"`             // адрес rathole-сервера для клиентов (host:port)
	JWT          string            `json:"jwt,omitempty"`           // токен авторизации
	Tunnels      map[string]Tunnel `json:"tunnels"`                 // name -> метаданные
	ActiveTunnel string            `json:"active_tunnel,omitempty"` // имя туннеля по умолчанию для publish без аргументов
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateName проверяет имя туннеля (латиница, цифры, дефис/подчёркивание).
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("некорректное имя туннеля %q: допускаются латиница, цифры, '-', '_' (1..64 символа)", name)
	}
	return nil
}

// Dir возвращает корневую директорию ~/.pnat (или PNAT_HOME для тестов).
func Dir() (string, error) {
	if h := os.Getenv("PNAT_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("не удалось определить домашнюю директорию: %w", err)
	}
	return filepath.Join(home, ".pnat"), nil
}

func mustWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("запись во временный файл %s: %w (проверьте права на запись в ~/.pnat)", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("переименование %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Load читает ~/.pnat/config.json. Если файла нет — возвращает пустое состояние
// с дефолтным сервером (первое сохранение создаст файл).
func Load() (*State, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	st := &State{ServerURL: DefaultServerURL, RemoteAddr: DefaultRemoteAddr, Tunnels: map[string]Tunnel{}}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, fmt.Errorf("чтение config.json: %w", err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("повреждён %s: %w", filepath.Join(dir, "config.json"), err)
	}
	if st.Tunnels == nil {
		st.Tunnels = map[string]Tunnel{}
	}
	if st.ServerURL == "" {
		st.ServerURL = DefaultServerURL
	}
	if st.RemoteAddr == "" {
		st.RemoteAddr = DefaultRemoteAddr
	}
	return st, nil
}

// Save атомарно пишет ~/.pnat/config.json (права 0600 — файл содержит JWT).
func (st *State) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("создание %s: %w (нет прав на запись?)", dir, err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return mustWrite(filepath.Join(dir, "config.json"), append(b, '\n'), 0o600)
}

// --- генерация единого client.toml для демона rathole ---

type tomlClient struct {
	RemoteAddr string `toml:"remote_addr"`
}

type tomlService struct {
	Type      string `toml:"type"`
	LocalAddr string `toml:"local_addr"`
	Token     string `toml:"token"`
}

type tomlConfig struct {
	Client  tomlClient             `toml:"client"`
	Services map[string]tomlService `toml:"client.services"`
}

// ClientTomlPath — путь к ~/.pnat/client.toml.
func ClientTomlPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client.toml"), nil
}

// RenderClientToml формирует содержимое единого конфига rathole-клиента:
// одна секция [client] и по секции [client.services.<имя>] на каждый туннель.
func RenderClientToml(remoteAddr string, tunnels map[string]Tunnel) (string, error) {
	var sb strings.Builder
	sb.WriteString("# Сгенерировано pnat CLI. НЕ РЕДАКТИРУЙТЕ ВРУЧНУЮ — изменения будут перезаписаны.\n")
	sb.WriteString("[client]\n")
	fmt.Fprintf(&sb, "remote_addr = %q\n", remoteAddr)
	names := make([]string, 0, len(tunnels))
	for n := range tunnels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := tunnels[n]
		if !t.Type.Valid() {
			return "", fmt.Errorf("туннель %q: неизвестный тип %q", n, t.Type)
		}
		if t.Token == "" || t.LocalAddr == "" {
			return "", fmt.Errorf("туннель %q: отсутствуют token или local_addr", n)
		}
		sb.WriteString("\n[client.services." + n + "]\n")
		fmt.Fprintf(&sb, "type = %q\n", string(t.Type))
		fmt.Fprintf(&sb, "local_addr = %q\n", t.LocalAddr)
		fmt.Fprintf(&sb, "token = %q\n", t.Token)
	}
	return sb.String(), nil
}

// WriteClientToml атомарно перезаписывает ~/.pnat/client.toml из состояния.
func (st *State) WriteClientToml() (string, error) {
	text, err := RenderClientToml(st.RemoteAddr, st.Tunnels)
	if err != nil {
		return "", err
	}
	// Самопроверка: сгенерированный TOML обязан корректно парситься.
	var probe map[string]any
	if _, err := toml.Decode(text, &probe); err != nil {
		return "", fmt.Errorf("сгенерирован невалидный TOML: %w", err)
	}
	path, err := ClientTomlPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("создание %s: %w", filepath.Dir(path), err)
	}
	if err := mustWrite(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// UpsertTunnel добавляет/обновляет туннель в состоянии.
func (st *State) UpsertTunnel(t Tunnel) error {
	if err := ValidateName(t.Name); err != nil {
		return err
	}
	if !t.Type.Valid() {
		return fmt.Errorf("недопустимый протокол %q (только tcp|udp)", t.Type)
	}
	st.Tunnels[t.Name] = t
	return nil
}

// RemoveTunnel удаляет туннель из состояния.
func (st *State) RemoveTunnel(name string) error {
	if _, ok := st.Tunnels[name]; !ok {
		return fmt.Errorf("туннель %q не найден в локальном состоянии", name)
	}
	delete(st.Tunnels, name)
	if st.ActiveTunnel == name {
		st.ActiveTunnel = ""
	}
	return nil
}
