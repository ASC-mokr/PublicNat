#!/usr/bin/env bash
# ============================================================
# PublicNat — деплой Control Plane + RatHole server на Debian 12 (VPS)
# Репозиторий: https://github.com/ASC-mokr/PublicNat
#
# Запускать как root на чистом VPS с Debian 12 (репозиторий ПРИВАТНЫЙ —
# настройте доступ заранее, см. deploy/PRIVATE_REPO.md):
#   git clone git@github.com:ASC-mokr/PublicNat.git /opt/publicnat/src
#   cd /opt/publicnat/src/deploy && sudo bash deploy.sh
#
# Идемпотентен: повторный запуск = git pull + пересборка + рестарт сервисов.
# Для приватного репозитория git-доступ берётся из окружения:
#   - SSH-ключ пользователя root (если origin клонирован по git@github.com:...)
#   - GIT_CONFIG_GLOBAL=/root/.gitconfig (если настроены insteadOf + PAT-креды)
# ============================================================
set -euo pipefail

APP_DIR=/opt/publicnat
SRC_DIR=$APP_DIR/src
BIN_DIR=$APP_DIR/bin
ENV_FILE=$APP_DIR/env.sh
GIT_URL=${GIT_URL:-https://github.com/ASC-mokr/PublicNat.git}
export GIT_CONFIG_GLOBAL=${GIT_CONFIG_GLOBAL:-/root/.gitconfig}
GO_VERSION=1.23.4
RATHOLE_FALLBACK_VERSION=v0.5.0 # последняя стабильная; активы v0.5.0: rathole-<target>.zip, имя БЕЗ версии

log()  { echo -e "\n\033[1;32m==> $*\033[0m"; }
die()  { echo -e "\033[1;31mERROR: $*\033[0m" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "Запускайте скрипт от root (sudo ./deploy.sh)"
[[ -f $ENV_FILE ]] || die "Не найден $ENV_FILE — скопируйте deploy/env.sh.example и заполните секреты"

# ---------- 1. Системные пакеты ----------
log "Установка системных пакетов (git, postgresql, curl, ufw)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq git curl ca-certificates gnupg postgresql postgresql-contrib ufw >/dev/null

# ---------- 2. Go (если отсутствует нужной версии) ----------
if [[ ! -x /usr/local/go/bin/go ]] || ! /usr/local/go/bin/go version | grep -q "go1\.\(2[3-9]\|[3-9][0-9]\)"; then
    log "Установка Go ${GO_VERSION}"
    curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
else
    log "Go уже установлен: $(/usr/local/go/bin/go version)"
fi
export PATH=/usr/local/go/bin:$PATH
export GOFLAGS=-buildvcs=false

# ---------- 3. Исходники (приватный репозиторий) ----------
log "Синхронизация исходников ASC-mokr/PublicNat в $SRC_DIR"
mkdir -p "$APP_DIR"
if [[ -d $SRC_DIR/.git ]]; then
    # Рабочая копия уже клонирована — обновляем до последней версии из GitHub.
    # fetch использует origin и креды из окружения (SSH-ключ root или
    # GIT_CONFIG_GLOBAL=/root/.gitconfig с insteadOf/PAT).
    if ! git -C "$SRC_DIR" fetch --all -q; then
        die "git fetch не выполнен. Приватный репозиторий: проверьте доступ (deploy/PRIVATE_REPO.md).
    Для SSH: ssh -T git@github.com; для HTTPS: cat /root/.gitconfig и /root/.git-credentials"
    fi
    BRANCH=$(git -C "$SRC_DIR" rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null | cut -d/ -f2 || echo main)
    git -C "$SRC_DIR" reset --hard -q "origin/${BRANCH:-main}"
elif [[ $(cd "$(dirname "$0")/.." && pwd) == $SRC_DIR ]]; then
    : # deploy.sh запущен прямо из целевой рабочей копии (git clone ... /opt/publicnat/src) — ничего не делаем
elif [[ -d $SRC_DIR ]]; then
    die "$SRC_DIR существует, но не является git-репозиторием"
else
    git clone -q "$GIT_URL" "$SRC_DIR" \
        || die "git clone не выполнен. Приватный репозиторий: сначала настройте доступ — см. deploy/PRIVATE_REPO.md"
fi
[[ -d $SRC_DIR/backend ]] || die "В репозитории нет каталога backend/"

# ---------- 4. Конфигурация приложения ----------
log "Проверка env.sh и создание systemd-пользователя"
if grep -n CHANGE_ME "$ENV_FILE" >/dev/null; then
    echo "Незаменённые значения CHANGE_ME найдены в строках:" >&2
    grep -n CHANGE_ME "$ENV_FILE" | sed 's/^/    /' >&2
    die "Замените все значения CHANGE_ME в $ENV_FILE (ВАЖНО: пароль в DATABASE_URL должен совпадать с PG_PASSWORD)"
fi
for var in PG_DB PG_USER PG_PASSWORD DATABASE_URL JWT_SECRET RATHOLE_CONFIG_PATH PORT_RANGE_START PORT_RANGE_END; do
    [[ -n $(grep -E "^${var}=" "$ENV_FILE" | tail -1 | cut -d= -f2-) ]] \
        || die "Переменная ${var} не задана или пуста в $ENV_FILE"
done
chmod 600 "$ENV_FILE"
id publicnat &>/dev/null || useradd --system --home "$APP_DIR" --shell /usr/sbin/nologin publicnat

# source env.sh для переменных деплоя (DATABASE_URL, порты и т.д.)
# shellcheck disable=SC1090
source "$ENV_FILE"

# ---------- 5. PostgreSQL: роль + база + миграции ----------
log "Настройка PostgreSQL (роль ${PG_USER:-publicnat}, БД ${PG_DB:-publicnat})"
systemctl enable --now postgresql

# SQL передаём через файл и psql -f: так надёжнее, чем экранирование кавычек
# внутри su -c (ошибка вида «DO 3420781 BEGIN» — это PID шелла, подставленный
# вместо $$ из-за двойного раскрытия переменных).
PGSQL_TMP=$(mktemp /tmp/publicnat-pg.XXXXXX.sql)
trap 'rm -f "$PGSQL_TMP"' EXIT
cat > "$PGSQL_TMP" <<EOF
DO \$body\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${PG_USER:-publicnat}') THEN
    CREATE ROLE ${PG_USER:-publicnat} LOGIN PASSWORD '$(sed "s/'/''/g" <<<"${PG_PASSWORD:?PG_PASSWORD не задан в env.sh}")';
  ELSE
    ALTER ROLE ${PG_USER:-publicnat} LOGIN PASSWORD '$(sed "s/'/''/g" <<<"${PG_PASSWORD:?PG_PASSWORD не задан в env.sh}")';
  END IF;
END
\$body\$
EOF
chmod 644 "$PGSQL_TMP"
if ! su - postgres -c "psql -v ON_ERROR_STOP=1 -q -f '$PGSQL_TMP'"; then
    die "Не удалось создать/обновить роль PostgreSQL"
fi
su - postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname='${PG_DB:-publicnat}'\"" | grep -q 1 \
    || su - postgres -c "createdb -O ${PG_USER:-publicnat} ${PG_DB:-publicnat}" \
    || die "Не удалось создать базу данных ${PG_DB:-publicnat}"

pg_hba_has_app() { grep -qE '^host\s+'"${PG_DB:-publicnat}"'\s+'"${PG_USER:-publicnat}"'\s+127\.0\.0\.1/32' /etc/postgresql/*/main/pg_hba.conf; }
if ! pg_hba_has_app; then
    HBA=$(ls /etc/postgresql/*/main/pg_hba.conf | sort -V | tail -1)
    printf 'host\t%s\t%s\t127.0.0.1/32\tscram-sha-256\nhost\t%s\t%s\t::1/128\tscram-sha-256\n' \
        "${PG_DB:-publicnat}" "${PG_USER:-publicnat}" "${PG_DB:-publicnat}" "${PG_USER:-publicnat}" >> "$HBA"
    log "Добавлены записи scram-sha-256 в $HBA"
fi
systemctl reload postgresql

# Миграции применяются и самим бэкэндом при старте — здесь только проверка соединения:
DB_HOST=$(sed -E 's|.*@([^:/]+).*|\1|' <<<"$DATABASE_URL")
DB_PORT=$(sed -E 's|.*@[^:/]+:([0-9]+).*|\1|' <<<"$DATABASE_URL")
PGPASSWORD=$PG_PASSWORD psql -h "$DB_HOST" -p "${DB_PORT:-5432}" -U "$PG_USER" -d "$PG_DB" -tAc "SELECT 1" >/dev/null \
    || die "Не удалось подключиться к PostgreSQL через DATABASE_URL"
log "PostgreSQL доступен. SQL-миграции применит бэкэнд при старте (backend/migrations/)"

# ---------- 6. Сборка Go-бэкэнда ----------
log "Сборка control-plane (go build)"
cd "$SRC_DIR/backend"
install -d -m 755 "$BIN_DIR"
go mod download
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN_DIR/control-plane.new" ./cmd/server
mv -f "$BIN_DIR/control-plane.new" "$BIN_DIR/control-plane"
chmod 755 "$BIN_DIR/control-plane"

# ---------- 7. RatHole server ----------
RV=${RATHOLE_VERSION:-$RATHOLE_FALLBACK_VERSION}
RATHOLE_BIN=$BIN_DIR/rathole
if [ -x "$RATHOLE_BIN" ] && "$RATHOLE_BIN" --version 2>/dev/null | grep -q "$RV"; then
    log "RatHole $RV уже установлен, пропуск загрузки"
else
    log "Установка бинарника rathole ($RV)"
    # В релизах rathole активы называются <asset>.zip и содержат ОДИН файл
    # rathole без каталога-обёртки (не .tar.gz!) — см. Assets на странице релиза.
    ASSET="rathole-x86_64-unknown-linux-gnu" # в v0.5.0 актив БЕЗ версии в имени (проверено HEAD 200)
    TMPD=$(mktemp -d)
    trap 'rm -rf "$TMPD"' EXIT
    curl -fL --retry 3 -o "$TMPD/rathole.zip" \
        "https://github.com/rathole-org/rathole/releases/download/${RV}/${ASSET}.zip" \
        || die "Не удалось скачать rathole ($RV). Список релизов: https://github.com/rathole-org/rathole/releases"
    command -v unzip >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq unzip; }
    unzip -q -o "$TMPD/rathole.zip" -d "$TMPD"
    RBIN=$(find "$TMPD" -name rathole -type f | head -n1)
    [ -n "$RBIN" ] || die "Бинарник rathole не найден в архиве"
    install -m 755 "$RBIN" "$RATHOLE_BIN"
    rm -rf "$TMPD"; trap - EXIT
fi
"$RATHOLE_BIN" --version || die "Скачанный rathole не запускается"

install -d -m 755 /etc/rathole
cp -f "$SRC_DIR/deploy/rathole/bootstrap-server.toml" /etc/rathole/server.toml
# RatHole не умеет писать pid-file — wrapper пишет PID сам, чтобы бэкэнд
# мог слать SIGHUP через RATHOLE_PIDFILE (см. internal/rathole/sync.go).
cat > /usr/local/bin/rathole-run <<EOF
#!/bin/bash
echo \$\$ > /run/rathole.pid
exec $RATHOLE_BIN /etc/rathole/server.toml
EOF
chmod 755 /usr/local/bin/rathole-run

chown -R publicnat:publicnat "$APP_DIR"
chown root:root /etc/rathole /etc/rathole/server.toml

# ---------- 8. systemd-юниты ----------
log "Установка systemd-юнитов"
install -m 644 "$SRC_DIR/deploy/systemd/publicnat-api.service"  /etc/systemd/system/
install -m 644 "$SRC_DIR/deploy/systemd/publicnat-rathole.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable publicnat-rathole publicnat-api

# ---------- 9. Firewall ----------
log "Настройка UFW (SSH, API 8080, RatHole 2333, туннели ${PORT_RANGE_START:-5000}-${PORT_RANGE_END:-6000})"
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw allow 22/tcp comment 'SSH' >/dev/null
ufw allow 8080/tcp comment 'PublicNat API' >/dev/null
ufw allow 2333/tcp comment 'RatHole client control' >/dev/null
ufw allow "${PORT_RANGE_START:-5000}:${PORT_RANGE_END:-6000}/tcp" comment 'PublicNat tunnels tcp' >/dev/null
ufw allow "${PORT_RANGE_START:-5000}:${PORT_RANGE_END:-6000}/udp" comment 'PublicNat tunnels udp' >/dev/null
ufw --force enable >/dev/null

# ---------- 10. Старт ----------
log "Запуск сервисов"
systemctl restart publicnat-rathole
sleep 1
systemctl restart publicnat-api
sleep 2
systemctl --no-pager status publicnat-rathole publicnat-api | grep -E "●|Active:" || true

log "Проверка API"
if curl -fsS -X POST http://localhost:8080/api/auth/register -H 'Content-Type: application/json' \
     -d '{"email":"healthcheck@local","password":"HealthCheck1!"}' >/dev/null 2>&1 \
   || curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
    echo "API отвечает."
else
    echo "ВНИМАНИЕ: API не ответил — смотрите journalctl -u publicnat-api -n 50"
fi

echo
echo -e "\033[1;32mДеплой завершён.\033[0m"
echo "  API:          http://$(hostname -I | awk '{print $1}'):8080"
echo "  RatHole:      $(hostname -I | awk '{print $1}'):${RATHOLE_BIND_ADDR##*:}"
echo "  Логи:         journalctl -u publicnat-api -f   |   journalctl -u publicnat-rathole -f"
echo "  Конфиг:       /etc/rathole/server.toml (генерируется бэкэндом, НЕ редактировать вручную)"
echo "  Обновление:   cd $SRC_DIR && git pull && $SRC_DIR/deploy/deploy.sh"
