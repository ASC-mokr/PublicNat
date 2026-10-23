// Package api — HTTP-клиент REST API бэкенда PublicNat.
// Только чистый net/http, без внешних утилит.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	token   string
	http    *http.Client
}

func New(baseURL, jwt string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		token:   jwt,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError — ошибка, возвращённая сервером (JSON {"error": "..."}).
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("сервер ответил %d: %s", e.Status, e.Msg) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("превышен таймаут ожидания ответа от %s (проверьте доступность VPS)", c.BaseURL)
		}
		var nerr *net.OpError
		if errors.As(err, &nerr) {
			return fmt.Errorf("сервер %s недоступен: %v (проверьте интернет и доступность VPS)", c.BaseURL, nerr)
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return fmt.Errorf("не удалось разрешить адрес %s: %v", c.BaseURL, dnsErr)
		}
		return fmt.Errorf("запрос %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("некорректный JSON от сервера (%s %s): %w", method, path, err)
		}
	}
	return nil
}

// --- модели ответов (совпадают с DTO бэкенда) ---

type TunnelDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	ServerPort  int    `json:"server_port"`
	Token       string `json:"tunnel_token"`
	Subdomain   string `json:"subdomain,omitempty"`
	ClientHint  string `json:"client_hint,omitempty"`
}

type LoginResponse struct {
	Token string `json:"token"`
	Email string `json:"email"`
}

// Login выполняет POST /api/auth/login.
func (c *Client) Login(ctx context.Context, email, password string) (*LoginResponse, error) {
	var out LoginResponse
	err := c.do(ctx, http.MethodPost, "/api/auth/login",
		map[string]string{"email": email, "password": password}, &out)
	if err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("сервер не вернул JWT")
	}
	return &out, nil
}

// CreateTunnel выполняет POST /api/tunnels (передаём ТОЛЬКО name и type).
func (c *Client) CreateTunnel(ctx context.Context, name, typ string) (*TunnelDTO, error) {
	var out TunnelDTO
	err := c.do(ctx, http.MethodPost, "/api/tunnels",
		map[string]string{"name": name, "type": typ}, &out)
	if err != nil {
		return nil, err
	}
	if out.ServerPort == 0 || out.Token == "" {
		return nil, fmt.Errorf("некорректный ответ сервера: пустой server_port или tunnel_token")
	}
	return &out, nil
}

// ListTunnels выполняет GET /api/tunnels.
func (c *Client) ListTunnels(ctx context.Context) ([]TunnelDTO, error) {
	var out struct {
		Tunnels []TunnelDTO `json:"tunnels"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/tunnels", nil, &out); err != nil {
		return nil, err
	}
	return out.Tunnels, nil
}

// DeleteTunnel выполняет DELETE /api/tunnels/:id. id может быть UUID или именем туннеля
// (бэкенд принимает оба варианта).
func (c *Client) DeleteTunnel(ctx context.Context, idOrName string) error {
	return c.do(ctx, http.MethodDelete, "/api/tunnels/"+idOrName, nil, nil)
}

// FindByName ищет туннель на сервере по имени.
func (c *Client) FindByName(ctx context.Context, name string) (*TunnelDTO, error) {
	list, err := c.ListTunnels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("туннель %q не найден на сервере", name)
}

// IsUnauthorized — признак того, что сервер отклонил JWT (401): нужен для
// подсказки «pnat login» в командах верхнего уровня.
func IsUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}
