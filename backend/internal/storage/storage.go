// Package storage — слой доступа к PostgreSQL (users, tunnels).
package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound — запись не найдена.
var ErrNotFound = errors.New("record not found")

// User — соответствует таблице users.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	IsActive     bool
	CreatedAt    time.Time
}

// Tunnel — соответствует таблице tunnels.
type Tunnel struct {
	ID          string
	UserID      string
	Name        string
	Type        string // tcp | udp
	ServerPort  int
	TunnelToken string
	Subdomain   *string
	CreatedAt   time.Time
}

// Store обёртка над пулом соединений pgx.
type Store struct {
	pool *pgxpool.Pool
}

// New создаёт пул соединений и выполняет миграции из директории migrationsDir.
func New(ctx context.Context, databaseURL, migrationsDir string) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("storage: parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("storage: connect pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping database: %w", err)
	}
	s := &Store{pool: pool}
	if migrationsDir != "" {
		if err := s.migrate(ctx, migrationsDir); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return s, nil
}

// migrate применяет все *.sql из директории миграций в алфавитном порядке.
func (s *Store) migrate(ctx context.Context, dir string) error {
	entries, err := readSQLFiles(dir)
	if err != nil {
		return fmt.Errorf("storage: read migrations: %w", err)
	}
	for _, m := range entries {
		if _, err := s.pool.Exec(ctx, m.Content); err != nil {
			return fmt.Errorf("storage: apply migration %s: %w", m.Name, err)
		}
	}
	return nil
}

// Close освобождает пул соединений.
func (s *Store) Close() { s.pool.Close() }

// Pool возвращает нижележащий пул (используется в тестах/апгрейде схемы).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ---------- Users ----------

func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, email, password_hash, is_active, created_at`,
		email, passwordHash,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsActive, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("storage: create user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, is_active, created_at FROM users WHERE email = $1`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsActive, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get user by email: %w", err)
	}
	return u, nil
}

// ---------- Tunnels ----------

// AllocatedPorts возвращает множество портов, уже занятых в БД
// (для быстрой проверки без N запросов при сканировании диапазона).
func (s *Store) AllocatedPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT server_port FROM tunnels`)
	if err != nil {
		return nil, fmt.Errorf("storage: allocated ports: %w", err)
	}
	defer rows.Close()
	used := make(map[int]bool)
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("storage: scan port: %w", err)
		}
		used[p] = true
	}
	return used, rows.Err()
}

// CreateTunnel вставляет туннель; уникальные ограничения (порт, имя пользователя, токен)
// обрабатываются вызывающим кодом через ошибки БД.
func (s *Store) CreateTunnel(ctx context.Context, t *Tunnel) (*Tunnel, error) {
	out := &Tunnel{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tunnels (user_id, name, type, server_port, tunnel_token, subdomain)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, user_id, name, type, server_port, tunnel_token, subdomain, created_at`,
		t.UserID, t.Name, t.Type, t.ServerPort, t.TunnelToken, t.Subdomain,
	).Scan(&out.ID, &out.UserID, &out.Name, &out.Type, &out.ServerPort, &out.TunnelToken, &out.Subdomain, &out.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("storage: create tunnel: %w", err)
	}
	return out, nil
}

// GetTunnelByID возвращает туннель по id.
func (s *Store) GetTunnelByID(ctx context.Context, id string) (*Tunnel, error) {
	t := &Tunnel{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, type, server_port, tunnel_token, subdomain, created_at
		 FROM tunnels WHERE id = $1`, id,
	).Scan(&t.ID, &t.UserID, &t.Name, &t.Type, &t.ServerPort, &t.TunnelToken, &t.Subdomain, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get tunnel: %w", err)
	}
	return t, nil
}

// ListTunnelsByUser — список туннелей конкретного пользователя.
func (s *Store) ListTunnelsByUser(ctx context.Context, userID string) ([]Tunnel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, type, server_port, tunnel_token, subdomain, created_at
		 FROM tunnels WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list tunnels: %w", err)
	}
	return scanTunnels(rows)
}

// ListAllTunnels — все туннели всех пользователей (для генерации server.toml).
func (s *Store) ListAllTunnels(ctx context.Context) ([]Tunnel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.user_id, t.name, t.type, t.server_port, t.tunnel_token, t.subdomain, t.created_at
		 FROM tunnels t
		 JOIN users u ON u.id = t.user_id
		 WHERE u.is_active = TRUE
		 ORDER BY t.server_port`)
	if err != nil {
		return nil, fmt.Errorf("storage: list all tunnels: %w", err)
	}
	return scanTunnels(rows)
}

// DeleteTunnel удаляет туннель по id и пользователю (защита от чужих объектов).
// Возвращает ErrNotFound, если ничего не удалено.
func (s *Store) DeleteTunnel(ctx context.Context, id, userID string) error {
	// id — UUID туннеля; допускаем и имя туннеля в рамках пользователя
	// (CLI оперирует именами): DELETE с составным условием через подзапрос.
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM tunnels t
		WHERE t.user_id = $2::uuid
		  AND (t.id::text = $1 OR t.name = $1)`,
		id, userID)
	if err != nil {
		return fmt.Errorf("storage: delete tunnel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanTunnels(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}) ([]Tunnel, error) {
	list := []Tunnel{}
	for rows.Next() {
		var t Tunnel
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Type, &t.ServerPort, &t.TunnelToken, &t.Subdomain, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan tunnel: %w", err)
		}
		list = append(list, t)
	}
	return list, rows.Err()
}
