# Деплой из приватного GitHub-репозитория (ASC-mokr/PublicNat)

`deploy.sh` выполняет `git fetch/clone` от имени **root**, поэтому доступ к
приватному репозиторию должен быть настроен для root на VPS. Выберите ОДИН
из двух вариантов.

---

## Вариант A — SSH-ключ (рекомендуется)

### 1. Сгенерируйте ключ на VPS (или используйте существующий)

```bash
ssh-keygen -t ed25519 -C "publicnat-vps" -f /root/.ssh/id_ed25519 -N ""
cat /root/.ssh/id_ed25519.pub
```

### 2. Добавьте публичный ключ в GitHub

- **На весь аккаунт:** Settings → SSH and GPG keys → New SSH key → вставьте public key.
- **Только на репозиторий (безопаснее):** PublicNat → Settings → Deploy keys →
  Add deploy key → вставьте ключ, галочку **Allow write access** НЕ ставить
  (для деплоя нужно только чтение).

### 3. Проверьте доступ

```bash
ssh -T git@github.com        # должно быть: Hi ASC-mokr! You've successfully authenticated...
```

### 4. Клонируйте по SSH-адресу

```bash
git clone git@github.com:ASC-mokr/PublicNat.git /opt/publicnat/src
cd /opt/publicnat/src/deploy && sudo bash deploy.sh
```

Дальше `deploy.sh` при повторных запусках сам делает `fetch + reset --hard`
через origin (SSH) — ключ уже работает, ничего больше не нужно.

Если клонировали раньше по HTTPS, переключите origin:

```bash
git -C /opt/publicnat/src remote set-url origin git@github.com:ASC-mokr/PublicNat.git
```

---

## Вариант B — HTTPS + Personal Access Token (PAT)

### 1. Создайте токен

GitHub → Settings → Developer settings → **Fine-grained personal access tokens**
(или Classic):
- Fine-grained: репозиторий PublicNat, разрешение **Contents: Read-only**.
- Classic: скоуп `repo`.

### 2. Сохраните креды и алиасы для root

```bash
chown root:root /root/.gitconfig /root/.git-credentials
chmod 600 /root/.git-credentials

# храним PAT в файле credentials (права 600!)
echo 'https://<ПАТ>:x-oauth-basic@github.com' > /root/.git-credentials

# git всегда брать креды из этого файла
git config --global credential.helper 'store --file=/root/.git-credentials'

# любые https-ссылки на этот репозиторий приводим к каноничному виду
git config --global url."https://github.com/".insteadOf "ssh://git@github.com/"
```

`deploy.sh` сам экспортирует `GIT_CONFIG_GLOBAL=/root/.gitconfig`, поэтому
эти настройки применяются даже при запуске через `sudo` из другого пользователя.

### 3. Клонирование/обновление

```bash
git clone https://github.com/ASC-mokr/PublicNat.git /opt/publicnat/src
cd /opt/publicnat/src/deploy && sudo bash deploy.sh
```

---

## Повседневное обновление кода

На рабочей машине (где у вас есть push-доступ):

```bash
git add -A && git commit -m "..." && git push origin main
```

На VPS — просто повторно запустить деплой (он идемпотентен: pull → build → restart):

```bash
sudo bash /opt/publicnat/src/deploy/deploy.sh
```

Или вручную, если нужен только pull без пересборки сервисов:

```bash
cd /opt/publicnat/src && git pull --ff-only
```

⚠️ Не делайте `git pull`, если локально на сервере правили файлы —
`deploy.sh` использует `reset --hard origin/<branch>` и затрёт локальные
изменения (это ожидаемое поведение: единственный источник истины — GitHub).

## Диагностика ошибок доступа

| Симптом | Причина / решение |
|---|---|
| `Permission denied (publickey)` | Ключ не добавлен в GitHub или не тот файл; проверьте `ssh-add -l`, `cat /root/.ssh/id_ed25519.pub` и список ключей в настройках |
| `fatal: could not read Username` | origin по HTTPS, а кредов нет — настройте Вариант B или переключите remote на SSH |
| `404 / Not Found` для существующего репо | У deploy-ключа нет доступа к этому репозиторию (Deploy keys привязаны к одному репо) |
| Токен протух (fine-grained имеет срок) | Замените значение в `/root/.git-credentials` |

## Безопасность

- Для деплоя достаточно **read-only** доступа (Deploy key или fine-grained
  токен с Contents: Read-only). Не храните на VPS токены с правом записи.
- `/root/.git-credentials` и `/root/.ssh/*` должны иметь права `600`.
- Если сервер отдаётся другим людям — удалите креды после настройки CI или
  используйте отдельный минимально привилегированный токен.
