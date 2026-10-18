// Package api — REST-эндпоинты управления туннелями и пользователями.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/example/nat-platform/backend/internal/auth"
	"github.com/example/nat-platform/backend/internal/config"
	"github.com/example/nat-platform/backend/internal/rathole"
	"github.com/example/nat-platform/backend/internal/storage"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ConfigSyncer — интерфейс синхронизации (упрощает тесты).
type ConfigSyncer interface {
	SyncRatholeConfig(ctx context.Context) error
}

// Server — HTTP-сервер control plane.
type Server struct {
	cfg    *config.Config
	store  *storage.Store
	syncer ConfigSyncer
	logger *slog.Logger
}

// NewServer собирает приложение.
func NewServer(cfg *config.Config, store *storage.Store, syncer ConfigSyncer, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, store: store, syncer: syncer, logger: logger}
}

// Routes строит chi-роутер со всеми эндпоинтами.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(s.loggingMiddleware)
	r.Use(chimw.Recoverer)

	r.Route("/api", func(r chi.Router) {
		r.Post("/auth/register", s.handleRegister)
		r.Post("/auth/login", s.handleLogin)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/tunnels", s.handleListTunnels)
			r.Post("/tunnels", s.handleCreateTunnel)
			r.Delete("/tunnels/{id}", s.handleDeleteTunnel)
		})
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return r
}

// ---------- Middleware / helpers ----------

type ctxKey string

const claimsKey ctxKey = "claims"

// requireAuth проверяет заголовок `Authorization: Bearer <jwt>`.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}
		claims, err := auth.ParseToken(s.cfg.JWTSecret, strings.TrimPrefix(h, prefix))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func claimsFrom(r *http.Request) *auth.Claims {
	c, _ := r.Context().Value(claimsKey).(*auth.Claims)
	return c
}

// loggingMiddleware — структурированное логирование каждого запроса.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.logger.Info("http request",
			"method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr, "request_id", chimw.GetReqID(r.Context()))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// isUniqueViolation определяет SQL-state 23xxx (unique/foreign key violation).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23")
}

// ---------- DTO ----------

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type createTunnelRequest struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"` // tcp | udp
	Subdomain *string `json:"subdomain,omitempty"`
}

type tunnelResponse struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	ServerPort  int     `json:"server_port"`
	TunnelToken string  `json:"tunnel_token"`
	Subdomain   *string `json:"subdomain,omitempty"`
	CreatedAt   string  `json:"created_at"`
	ClientHint  string  `json:"client_hint"` // готовый пример секции client.toml
}

func toResponse(t storage.Tunnel, serverBind string) tunnelResponse {
	hint := fmt.Sprintf(
		"[client.services.%s]\ntoken = \"%s\"\nremote_addr = \"<VPS_IP>:%d\"",
		t.Name, t.TunnelToken, t.ServerPort)
	return tunnelResponse{
		ID: t.ID, Name: t.Name, Type: t.Type,
		ServerPort: t.ServerPort, TunnelToken: t.TunnelToken,
		Subdomain: t.Subdomain, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		ClientHint: fmt.Sprintf("# RatHole server: %s\n%s", serverBind, hint),
	}
}

// ---------- Auth handlers ----------

// POST /api/auth/register
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(email, "@") || len(email) > 255 {
		writeError(w, http.StatusBadRequest, "invalid email")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.logger.Error("register: hash password failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	u, err := s.store.CreateUser(r.Context(), email, hash)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "email already registered")
			return
		}
		s.logger.Error("register: db insert failed", "error", err, "email", email)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.logger.Info("user registered", "user_id", u.ID, "email", email)
	token, err := auth.GenerateToken(s.cfg.JWTSecret, u.ID, u.Email,
		time.Duration(s.cfg.JWTTTLHrs)*time.Hour)
	if err != nil {
		s.logger.Error("register: token generation failed", "error", err, "user_id", u.ID)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": u.ID, "email": u.Email, "token": token,
	})
}

// POST /api/auth/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	u, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		s.logger.Error("login: db query failed", "error", err, "email", email)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !u.IsActive {
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := auth.GenerateToken(s.cfg.JWTSecret, u.ID, u.Email,
		time.Duration(s.cfg.JWTTTLHrs)*time.Hour)
	if err != nil {
		s.logger.Error("login: token generation failed", "error", err, "user_id", u.ID)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.logger.Info("user logged in", "user_id", u.ID, "email", email)
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "email": u.Email, "token": token})
}

// ---------- Tunnel handlers ----------

// POST /api/tunnels — выбор свободного порта, генерация токена, запись в БД, синхронизация конфига.
func (s *Server) handleCreateTunnel(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var req createTunnelRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := rathole.ValidateTunnelName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	typ := strings.ToLower(strings.TrimSpace(req.Type))
	if typ != "tcp" && typ != "udp" {
		writeError(w, http.StatusBadRequest, "type must be 'tcp' or 'udp'")
		return
	}
	var sub *string
	if req.Subdomain != nil {
		v := strings.ToLower(strings.TrimSpace(*req.Subdomain))
		if v != "" {
			if err := rathole.ValidateSubdomain(v); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			sub = &v
		}
	}

	tokenStr, err := rathole.GenerateToken(48) // криптографически стойкий токен >= 32 символов
	if err != nil {
		s.logger.Error("create tunnel: token generation failed", "error", err, "user_id", claims.UserID)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Атомарный выбор порта + вставка под advisory lock, чтобы два параллельных
	// запроса не выбрали один и тот же свободный порт.
	tunnel, err := s.createTunnelWithPort(r.Context(), claims.UserID, name, typ, sub, tokenStr)
	if err != nil {
		switch {
		case errors.Is(err, errNoFreePorts):
			writeError(w, http.StatusServiceUnavailable, "no free ports in allowed range")
		case isUniqueViolation(err):
			writeError(w, http.StatusConflict, "tunnel name or subdomain already exists")
		default:
			s.logger.Error("create tunnel: db failed", "error", err, "user_id", claims.UserID, "name", name)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	s.logger.Info("tunnel created", "tunnel_id", tunnel.ID, "user_id", claims.UserID,
		"name", tunnel.Name, "port", tunnel.ServerPort, "type", tunnel.Type)

	// Поток обновления конфигурации (критический узел).
	if err := s.syncer.SyncRatholeConfig(r.Context()); err != nil {
		// Конфиг не применён — откатываем запись, чтобы БД и RatHole не разъезжались.
		s.logger.Error("create tunnel: config sync failed, rolling back",
			"error", err, "tunnel_id", tunnel.ID)
		if delErr := s.store.DeleteTunnel(r.Context(), tunnel.ID, claims.UserID); delErr != nil {
			s.logger.Error("create tunnel: rollback delete failed", "error", delErr, "tunnel_id", tunnel.ID)
		}
		writeError(w, http.StatusInternalServerError, "failed to apply rathole configuration")
		return
	}

	writeJSON(w, http.StatusCreated, toResponse(*tunnel, s.cfg.RatholeBindAddr))
}

var errNoFreePorts = errors.New("no free ports in range")

// createTunnelWithPort подбирает свободный порт из диапазона и вставляет туннель в транзакции,
// сериализуя подбор через pg advisory lock.
func (s *Server) createTunnelWithPort(ctx context.Context, userID, name, typ string, sub *string, token string) (*storage.Tunnel, error) {
	pool := s.store.Pool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ключ фиксированный — все создания туннелей сериализуются на одном advisory lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rathole_port_alloc'))`); err != nil {
		return nil, fmt.Errorf("api: advisory lock: %w", err)
	}

	used, err := s.allocatedPortsInTx(ctx, tx)
	if err != nil {
		return nil, err
	}

	port, err := s.pickFreePort(used)
	if err != nil {
		return nil, err
	}

	var out storage.Tunnel
	err = tx.QueryRow(ctx,
		`INSERT INTO tunnels (user_id, name, type, server_port, tunnel_token, subdomain)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING id, user_id, name, type, server_port, tunnel_token, subdomain, created_at`,
		userID, name, typ, port, token, sub,
	).Scan(&out.ID, &out.UserID, &out.Name, &out.Type, &out.ServerPort, &out.TunnelToken, &out.Subdomain, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("api: commit: %w", err)
	}
	return &out, nil
}

func (s *Server) allocatedPortsInTx(ctx context.Context, tx pgx.Tx) (map[int]bool, error) {
	rows, err := tx.Query(ctx, `SELECT server_port FROM tunnels`)
	if err != nil {
		return nil, fmt.Errorf("api: query used ports: %w", err)
	}
	defer rows.Close()
	used := make(map[int]bool)
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("api: scan used port: %w", err)
		}
		used[p] = true
	}
	return used, rows.Err()
}

// pickFreePort перебирает диапазон PORT_RANGE_START..PORT_RANGE_END:
//  1. пропускает порты, занятые другими туннелями (БД);
//  2. проверяет, что порт физически свободен на хосте (валидация перед записью).
func (s *Server) pickFreePort(used map[int]bool) (int, error) {
	for p := s.cfg.PortRangeStart; p <= s.cfg.PortRangeEnd; p++ {
		if used[p] {
			continue
		}
		if s.cfg.SystemPortCheck && !isHostPortFree(p) {
			s.logger.Warn("port occupied by system service, skipping", "port", p)
			continue
		}
		return p, nil
	}
	return 0, errNoFreePorts
}

// isHostPortFree пытается bind'нуться на TCP и UDP — так мы валидируем,
// что порт не занят другой системной службой до записи в БД/конфиг.
func isHostPortFree(port int) bool {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = tcpLn.Close()
	udpConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	_ = udpConn.Close()
	return true
}

// DELETE /api/tunnels/{id}
func (s *Server) handleDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing tunnel id")
		return
	}
	if err := s.store.DeleteTunnel(r.Context(), id, claims.UserID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tunnel not found")
			return
		}
		s.logger.Error("delete tunnel: db failed", "error", err, "tunnel_id", id, "user_id", claims.UserID)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.logger.Info("tunnel deleted", "tunnel_id", id, "user_id", claims.UserID)

	if err := s.syncer.SyncRatholeConfig(r.Context()); err != nil {
		s.logger.Error("delete tunnel: config sync failed", "error", err, "tunnel_id", id)
		// Запись уже удалена; сигналим, что RatHole ещё держит старый конфиг.
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":  "deleted",
			"warning": "rathole config reload failed; retry later",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GET /api/tunnels
func (s *Server) handleListTunnels(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	list, err := s.store.ListTunnelsByUser(r.Context(), claims.UserID)
	if err != nil {
		s.logger.Error("list tunnels: db failed", "error", err, "user_id", claims.UserID)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := make([]tunnelResponse, 0, len(list))
	for _, t := range list {
		resp = append(resp, toResponse(t, s.cfg.RatholeBindAddr))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnels": resp, "count": len(resp)})
}
