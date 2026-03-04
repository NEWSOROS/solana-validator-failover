# solana-validator-failover

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**[English README](README.md)**

P2P-инструмент для **плановых** переключений Solana-валидаторов. Для **автоматических** (незапланированных) фейловеров см. [solana-validator-ha](https://github.com/SOL-Strategies/solana-validator-ha).

QUIC-программа для безопасного и быстрого переключения identity между Solana-валидаторами:

1. Активный валидатор устанавливает passive identity
2. Tower-файл синхронизируется через QUIC от активного к пассивному
3. Пассивный валидатор устанавливает active identity

## Два режима

| Команда | Назначение | Запускается на |
|---------|-----------|----------------|
| `switchover` | **Оркестратор** — обнаруживает кластер, проверяет состояние, выполняет переключение через SSH | Любая одна нода |
| `run` | **Низкоуровневый** QUIC клиент/сервер — вызывается `switchover` автоматически или запускается вручную | Обе ноды отдельно |

## Быстрый старт

### Оркестрированное переключение (рекомендуется, v0.2.0+)

Запускается с **любой** ноды кластера. Бинарь определяет роли через RPC, проверяет здоровье и оркестрирует всё через SSH:

```bash
# Интерактивное меню — дашборд, preflight, выбор действия
solana-validator-failover switchover -c config.yaml

# Dry-run — симуляция без переключения identity
solana-validator-failover switchover -c config.yaml --dry-run --yes

# Live-переключение — без промптов, выполняется сразу
solana-validator-failover switchover -c config.yaml --yes
```

### Ручное переключение (низкоуровневое)

Запускается `run` на **обеих нодах** отдельно. Сначала passive:

```bash
# Шаг 1: На PASSIVE ноде — запуск QUIC-сервера, ожидание active
solana-validator-failover run -c config.yaml --yes --not-a-drill

# Шаг 2: На ACTIVE ноде — подключение как QUIC-клиент, инициация переключения
solana-validator-failover run -c config.yaml --yes --to-peer backup-1 --not-a-drill
```

> Без `--not-a-drill` команда `run` работает в **dry-run режиме**: tower синхронизируется, тайминги записываются, identity НЕ переключается.

## Установка

Скачать из [релизов](https://github.com/NEWSOROS/solana-validator-failover/releases):

```bash
VERSION=0.2.0
wget https://github.com/NEWSOROS/solana-validator-failover/releases/download/v${VERSION}/solana-validator-failover-${VERSION}-linux-amd64.gz
gunzip solana-validator-failover-${VERSION}-linux-amd64.gz
chmod +x solana-validator-failover-${VERSION}-linux-amd64
sudo mv solana-validator-failover-${VERSION}-linux-amd64 /usr/local/bin/solana-validator-failover
```

Или сборка из исходников: `make build`

## Конфигурация

```yaml
validator:
  bin: agave-validator
  cluster: mainnet-beta
  public_ip: "1.2.3.4"
  hostname: "primary-1"

  identities:
    active: /path/to/validator-keypair.json
    active_pubkey: "ABC123..."
    passive: /path/to/validator-unstaked-keypair.json
    passive_pubkey: "DEF456..."

  ledger_dir: /mnt/solana/ledger
  rpc_address: http://localhost:8899

  tower:
    dir: /mnt/solana/ramdisk/tower
    auto_empty_when_passive: false
    file_name_template: "tower-1_9-{{ .Identities.Active.PubKey }}.bin"

  failover:
    server:
      port: 9898                # QUIC (UDP) порт
    min_time_to_leader_slot: 5m # мин. время до лидер-слота

    peers:
      backup-1:
        address: "5.6.7.8:9898"
        ssh_user: solana            # для switchover (по умолчанию: solana)
        ssh_key: ~/.ssh/id_ed25519  # для switchover (обязательно)
        ssh_port: 22                # для switchover (по умолчанию: 22)

    switchover:
      max_slot_lag: 100                           # макс. разница слотов (по умолчанию: 100)
      failover_binary: solana-validator-failover  # бинарь на удалённых нодах

    monitor:
      credit_samples:
        count: 5      # кол-во сэмплов vote credits
        interval: 5s  # интервал между сэмплами
```

## Команды

### `switchover`

Оркестрирует переключение целиком с одной ноды:

1. **Discovery** — опрашивает локальный RPC + пиров через SSH, определяет роли ACTIVE/PASSIVE
2. **Dashboard** — таблица состояния кластера (ноды, IP, роли, здоровье, слоты)
3. **Preflight** — проверяет здоровье, slot lag, SSH-доступность
4. **Menu** — интерактивный выбор действия (или авто-запуск с `--yes`)
5. **Execute** — запускает `run` на обеих сторонах (локально + через SSH)
6. **Verify** — повторно проверяет состояние кластера после переключения

**Автоопределение сценария:**

| Запуск с | QUIC Server | QUIC Client |
|----------|-------------|-------------|
| PASSIVE ноды | Локально | SSH → active нода (фон) |
| ACTIVE ноды | SSH → passive нода (фон) | Локально |
| Внешней ноды | SSH → passive нода (фон) | SSH → active нода (стриминг) |

```
switchover [флаги]
  --dry-run          Симуляция без переключения identity
  -y, --yes          Пропустить интерактивные промпты
  --to-peer <имя>    Указать целевой пир
```

### `run`

Низкоуровневый QUIC фейловер. Автоматически определяет роль из gossip:

- **Passive нода** → запускает QUIC-сервер, ждёт подключения active
- **Active нода** → подключается как QUIC-клиент к passive

```
run [флаги]
  --not-a-drill                Выполнить по-настоящему (по умолчанию: dry-run)
  --no-wait-for-healthy        Не ждать пока нода будет healthy
  --no-min-time-to-leader-slot Не ждать отсутствия лидер-слотов
  --skip-tower-sync            Пропустить синхронизацию tower-файла
  -y, --yes                    Пропустить промпты подтверждения
  --to-peer <имя|ip>           Авто-выбор пира (только на active ноде)
```

### Глобальные флаги

```
  -c, --config <путь>       Конфиг-файл (по умолчанию: ~/solana-validator-failover/solana-validator-failover.yaml)
  -l, --log-level <уровень> Уровень логов: debug, info, warn, error (по умолчанию: info)
```

## Порты

| Порт | Протокол | Назначение |
|------|----------|------------|
| 9898 | UDP | QUIC-коммуникация для фейловера |

## Требования

- Низколатентный UDP-маршрут между валидаторами (для QUIC)
- Беспарольный SSH между нодами (для команды `switchover`)
- Identity-ключи развёрнуты на каждой ноде

## Разработка

```bash
make dev           # Docker-среда с live-reload
make test          # Запуск тестов
make build         # Локальная сборка
make build-compose # Сборка через Docker (multi-arch)
```
