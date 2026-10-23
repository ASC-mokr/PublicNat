// Package cmd — CLI-команды pnat на github.com/spf13/cobra.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/example/nat-platform/cli/internal/api"
	"github.com/example/nat-platform/cli/internal/config"
	"github.com/example/nat-platform/cli/internal/daemon"
)

var version = "dev"

// Execute — точка входа из main.go. Возвращает код выхода.
func Execute() int {
	root := &cobra.Command{
		Use:           "pnat",
		Short:         "PublicNat — клиент публикации локальных портов через NAT (RatHole)",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(loginCmd(), publishCmd(), statusCmd(), stopCmd(), logoutCmd(), daemonCmd())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "[Ошибка]", err)
		return 1
	}
	return 0
}

func readLine(prompt string) (string, error) {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("чтение ввода: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// readPasswordSafe читает пароль без эха; при отсутствии TTY принимает переменную
// PNAT_PASSWORD (для скриптов/CI).
func readPasswordSafe(prompt string) (string, error) {
	if pw := os.Getenv("PNAT_PASSWORD"); pw != "" && !term.IsTerminal(int(os.Stdin.Fd())) {
		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) { // stdin перенаправлен — читаем строку
		return readLine(prompt)
	}
	fmt.Print(prompt)
	b, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("ввод пароля: %w", err)
	}
	return string(b), nil
}

func requireJWT(st *config.State) error {
	if st.JWT == "" {
		return errors.New("вы не авторизованы — сначала выполните `pnat login`")
	}
	return nil
}

func apiClient(st *config.State) *api.Client { return api.New(st.ServerURL, st.JWT) }

func withAuthHint(err error) error {
	if api.IsUnauthorized(err) {
		return fmt.Errorf("%w\nПодсказка: сессия истекла — выполните `pnat login` повторно", err)
	}
	return err
}

// --- pnat login ---

func loginCmd() *cobra.Command {
	var email, password string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Авторизация в PublicNat (сохраняет JWT в ~/.pnat/config.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var err error
			if email == "" {
				email, err = readLine("Email: ")
				if err != nil {
					return err
				}
			}
			if password == "" {
				password, err = readPasswordSafe("Пароль: ")
				if err != nil {
					return err
				}
			}
			if email == "" || password == "" {
				return errors.New("email и пароль обязательны")
			}
			st, err := config.Load()
			if err != nil {
				return err
			}
			resp, err := apiClient(st).Login(ctx, email, password)
			if err != nil {
				return err
			}
			st.JWT = resp.Token
			if err := st.Save(); err != nil {
				return err
			}
			fmt.Printf("[Успешно] Авторизован как %s. Токен сохранён в ~/.pnat/config.json\n", resp.Email)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email (если не указан — запросить)")
	cmd.Flags().StringVar(&password, "password", "", "пароль (небезопасно; лучше интерактивный ввод или PNAT_PASSWORD)")
	return cmd
}

// --- pnat logout ---

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Удалить сохранённый JWT",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := config.Load()
			if err != nil {
				return err
			}
			st.JWT = ""
			if err := st.Save(); err != nil {
				return err
			}
			fmt.Println("[Успешно] Сессия завершена")
			return nil
		},
	}
}

// --- pnat publish <имя> [протокол] [локальный_порт] ---

func publishCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "publish <имя_туннеля> [протокол] [локальный_порт]",
		Short: "Создать туннель и опубликовать локальный порт",
		Long: `Создаёт туннель на сервере (POST /api/tunnels передаются ТОЛЬКО name и type),
сохраняет server_port и tunnel_token в ~/.pnat/config.json, перегенерирует
единый ~/.pnat/client.toml и перезапускает демон rathole (hot-reload у клиента нет).

Пример: pnat publish web tcp 8080`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			if err := config.ValidateName(name); err != nil {
				return err
			}

			st, err := config.Load()
			if err != nil {
				return err
			}
			if err := requireJWT(st); err != nil {
				return err
			}

			// Протокол и локальный порт: из аргументов или из ранее сохранённых метаданных.
			typ := ""
			localAddr := ""
			if prev, ok := st.Tunnels[name]; ok {
				typ = string(prev.Type)
				localAddr = prev.LocalAddr
			}
			switch len(args) {
			case 1:
				if typ == "" || localAddr == "" {
					return fmt.Errorf("туннель %q ещё не настроен локально — укажите протокол и порт: pnat publish %s tcp 8080", name, name)
				}
				fmt.Printf("[..] Повторный запуск туннеля %q: протокол=%s, локальный адрес=%s\n", name, typ, localAddr)
			case 3:
				typ = strings.ToLower(args[1])
				port, err := strconv.Atoi(args[2])
				if err != nil || port < 1 || port > 65535 {
					return fmt.Errorf("некорректный локальный порт %q (ожидается 1..65535)", args[2])
				}
				localAddr = "127.0.0.1:" + strconv.Itoa(port)
			default:
				return errors.New("формат: pnat publish <имя> <tcp|udp> <локальный_порт>")
			}
			ttype := config.TunnelType(typ)
			if !ttype.Valid() {
				return fmt.Errorf("недопустимый протокол %q (только tcp или udp)", typ)
			}

			client := apiClient(st)

			// 1. Публикация на сервере (idempotent: если имя уже есть на сервере — используем его).
			dto, err := client.CreateTunnel(ctx, name, string(ttype))
			if ae := new(api.APIError); errors.As(err, &ae) && ae.Status == 409 {
				dto, err = client.FindByName(ctx, name)
			}
			if err != nil {
				return withAuthHint(err)
			}

			// 2. Сохраняем метаданные в ~/.pnat/config.json.
			tun := config.Tunnel{Name: dto.Name, Type: ttype, ServerPort: dto.ServerPort, Token: dto.Token, LocalAddr: localAddr}
			if dto.Type != "" {
				tun.Type = config.TunnelType(dto.Type)
			}
			if err := st.UpsertTunnel(tun); err != nil {
				return err
			}
			st.ActiveTunnel = name
			if err := st.Save(); err != nil {
				return err
			}

			// 3. Пересобираем единый client.toml (все туннели) и перезапускаем демон.
			tomlPath, err := st.WriteClientToml()
			if err != nil {
				return err
			}
			if err := daemon.Restart(tomlPath); err != nil {
				return err
			}

			host := hostOf(st.RemoteAddr)
			fmt.Printf("[Успешно] Туннель создан. Порт %s доступен по адресу %s:%d\n",
				portOf(localAddr), host, dto.ServerPort)
			return nil
		},
	}
}

// --- pnat status ---

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"ls", "list"},
		Short:   "Список туннелей и статус локального демона",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := config.Load()
			if err != nil {
				return err
			}
			if err := requireJWT(st); err != nil {
				return err
			}
			list, err := apiClient(st).ListTunnels(ctx)
			if err != nil {
				return withAuthHint(err)
			}

			dm := daemon.Status_()
			if dm.Running {
				fmt.Printf("Демон rathole: запущен (PID %d)\n", dm.PID)
			} else if dm.PID != 0 {
				fmt.Printf("Демон rathole: НЕ запущен (PID %d мёртв — выполните pnat publish для перезапуска)\n", dm.PID)
			} else {
				fmt.Println("Демон rathole: остановлен")
			}
			fmt.Printf("Сервер API: %s | Control-канал: %s\n\n", st.ServerURL, st.RemoteAddr)

			if len(list) == 0 {
				fmt.Println("Туннелей нет. Создайте первый: pnat publish web tcp 8080")
				return nil
			}
			tw := tablewriter.NewWriter(os.Stdout)
			tw.SetHeader([]string{"ИМЯ", "ПРОТОКОЛ", "ПУБЛИКАЦИЯ", "ЛОКАЛЬНО", "ДЕМОН"})
			for _, t := range list {
				local := "-"
				if lt, ok := st.Tunnels[t.Name]; ok && lt.LocalAddr != "" {
					local = lt.LocalAddr
				}
				state := "нет конфига"
				if _, ok := st.Tunnels[t.Name]; ok {
					if dm.Running {
						state = "активен"
					} else {
						state = "остановлен"
					}
				}
				tw.Append([]string{t.Name, strings.ToUpper(t.Type),
					fmt.Sprintf("%s:%d", hostOf(st.RemoteAddr), t.ServerPort), local, state})
			}
			tw.Render()
			return nil
		},
	}
}

// --- pnat stop <имя_туннеля> ---

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <имя_туннеля>",
		Short: "Удалить туннель с сервера и остановить его локально",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			st, err := config.Load()
			if err != nil {
				return err
			}
			if err := requireJWT(st); err != nil {
				return err
			}
			client := apiClient(st)

			// 1. Удаляем на сервере (по UUID из локального состояния либо по имени).
			idOrName := name
			if dto, err := client.FindByName(ctx, name); err == nil {
				idOrName = dto.ID
			} else if !strings.Contains(err.Error(), "не найден") {
				return withAuthHint(err)
			}
			if err := client.DeleteTunnel(ctx, idOrName); err != nil {
				var ae *api.APIError
				if errors.As(err, &ae) && ae.Status == 404 {
					fmt.Printf("[Успешно] Туннель %q отсутствовал на сервере — убираю локальные данные\n", name)
				} else {
					return withAuthHint(err)
				}
			}

			// 2. Удаляем секцию из локального состояния и client.toml.
			if err := st.RemoveTunnel(name); err != nil {
				fmt.Fprintf(os.Stderr, "Внимание: %v\n", err)
			}
			if err := st.Save(); err != nil {
				return err
			}
			tomlPath, err := st.WriteClientToml()
			if err != nil {
				return err
			}

			// 3. Перезапускаем демон с обновлённым конфигом (или останавливаем, если туннелей не осталось).
			if len(st.Tunnels) == 0 {
				if stopped, err := daemon.Stop(); err != nil {
					return err
				} else if stopped {
					fmt.Println("[Успешно] Туннелей не осталось — демон rathole остановлен")
				} else {
					fmt.Println("[Успешно] Туннелей не осталось — демон не запущен")
				}
			} else {
				if err := daemon.Restart(tomlPath); err != nil {
					return err
				}
			}
			fmt.Printf("[Успешно] Туннель %q удалён\n", name)
			return nil
		},
	}
}

// --- pnat daemon start|stop|restart (вспомогательная) ---

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Управление локальным демоном rathole",
	}
	path := func() (string, error) {
		st, err := config.Load()
		if err != nil {
			return "", err
		}
		if len(st.Tunnels) == 0 {
			return "", errors.New("нет ни одного туннеля — сначала выполните `pnat publish ...`")
		}
		return st.WriteClientToml()
	}
	start := &cobra.Command{Use: "start", Short: "Запустить демон", RunE: func(c *cobra.Command, a []string) error {
		p, err := path()
		if err != nil {
			return err
		}
		return daemon.Restart(p)
	}}
	restart := &cobra.Command{Use: "restart", Short: "Перезапустить демон", RunE: start.RunE}
	stop := &cobra.Command{Use: "stop", Short: "Остановить демон", RunE: func(c *cobra.Command, a []string) error {
		stopped, err := daemon.Stop()
		if err != nil {
			return err
		}
		if stopped {
			fmt.Println("[Успешно] Демон остановлен")
		} else {
			fmt.Println("Демон не запущен")
		}
		return nil
	}}
	status := &cobra.Command{Use: "status", Short: "Статус демона", RunE: func(c *cobra.Command, a []string) error {
		s := daemon.Status_()
		if s.Running {
			fmt.Printf("запущен, PID %d (%s)\n", s.PID, s.Exe)
		} else {
			fmt.Println("остановлен")
		}
		return nil
	}}
	cmd.AddCommand(start, restart, stop, status)
	return cmd
}

func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func portOf(localAddr string) string {
	if i := strings.LastIndex(localAddr, ":"); i >= 0 {
		return localAddr[i+1:]
	}
	return localAddr
}
