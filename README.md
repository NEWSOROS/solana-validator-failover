# solana-validator-failover

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**[README на русском](README.ru.md)**

P2P tool for **planned** Solana validator switchovers. For **automatic** (unplanned) failovers, see [solana-validator-ha](https://github.com/SOL-Strategies/solana-validator-ha).

QUIC-based program that orchestrates safe, fast identity switches between Solana validators:

1. Active validator sets identity to passive
2. Tower file synced via QUIC from active to passive
3. Passive validator sets identity to active

## Two modes

| Command | Purpose | Runs on |
|---------|---------|---------|
| `switchover` | **Orchestrator** — discovers cluster state, runs preflight checks, executes switchover via SSH | Any single node |
| `run` | **Low-level** QUIC client/server — called by `switchover` automatically, or run manually on both nodes | Both nodes separately |

## Quick Start

### Orchestrated switchover (recommended, v0.2.0+)

Run from **any** node in the cluster. The binary discovers roles via RPC, checks health, and orchestrates everything via SSH:

```bash
# Interactive menu — shows dashboard, preflight, lets you choose action
solana-validator-failover switchover -c config.yaml

# Dry-run — simulates everything without switching identity
solana-validator-failover switchover -c config.yaml --dry-run --yes

# Live switchover — no prompts, executes immediately
solana-validator-failover switchover -c config.yaml --yes
```

### Manual switchover (low-level)

Run `run` on **both nodes** separately. Start passive first:

```bash
# Step 1: On PASSIVE node — starts QUIC server, waits for active
solana-validator-failover run -c config.yaml --yes --not-a-drill

# Step 2: On ACTIVE node — connects as QUIC client, initiates handover
solana-validator-failover run -c config.yaml --yes --to-peer backup-1 --not-a-drill
```

> Without `--not-a-drill`, `run` executes in **dry-run mode**: tower synced, timings recorded, identity NOT switched.

## Installation

Download from [releases](https://github.com/NEWSOROS/solana-validator-failover/releases):

```bash
VERSION=0.2.0
wget https://github.com/NEWSOROS/solana-validator-failover/releases/download/v${VERSION}/solana-validator-failover-${VERSION}-linux-amd64.gz
gunzip solana-validator-failover-${VERSION}-linux-amd64.gz
chmod +x solana-validator-failover-${VERSION}-linux-amd64
sudo mv solana-validator-failover-${VERSION}-linux-amd64 /usr/local/bin/solana-validator-failover
```

Or build from source: `make build`

## Configuration

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
      port: 9898
    min_time_to_leader_slot: 5m

    peers:
      backup-1:
        address: "5.6.7.8:9898"
        ssh_user: solana          # for switchover command (default: solana)
        ssh_key: ~/.ssh/id_ed25519 # for switchover command (required)
        ssh_port: 22              # for switchover command (default: 22)

    switchover:
      max_slot_lag: 100                           # max slot difference (default: 100)
      failover_binary: solana-validator-failover  # binary on remote nodes (default)

    set_identity_active_cmd_template: "{{ .Bin }} --ledger {{ .LedgerDir }} set-identity {{ .Identities.Active.KeyFile }} --require-tower"
    set_identity_passive_cmd_template: "{{ .Bin }} --ledger {{ .LedgerDir }} set-identity {{ .Identities.Passive.KeyFile }}"

    monitor:
      credit_samples:
        count: 5
        interval: 5s

    # hooks: (optional) see full example below
```

## Commands

### `switchover`

Orchestrates the entire switchover from a single node:

1. **Discovery** — queries local RPC + remote peers via SSH, detects ACTIVE/PASSIVE roles
2. **Dashboard** — renders cluster state table (nodes, IPs, roles, health, slots, slot lag)
3. **Preflight** — validates health, slot lag, SSH connectivity
4. **Menu** — interactive action selection (or auto-proceed with `--yes`)
5. **Execute** — launches `run` on both sides (locally + via SSH)
6. **Verify** — re-queries cluster state after switchover

**Auto-detected execution cases:**

| Running from | QUIC Server | QUIC Client |
|-------------|-------------|-------------|
| PASSIVE node | Local subprocess | SSH → active node (background) |
| ACTIVE node | SSH → passive node (background) | Local subprocess |
| External node | SSH → passive node (background) | SSH → active node (streaming) |

```
switchover [flags]
  --dry-run          Simulate without switching identities
  -y, --yes          Skip interactive prompts
  --to-peer <name>   Target peer name
```

### `run`

Low-level QUIC failover. Auto-detects role from gossip:

- **Passive node** → starts QUIC server, waits for active to connect
- **Active node** → connects to passive peer as QUIC client

```
run [flags]
  --not-a-drill                Execute for real (default: dry-run)
  --no-wait-for-healthy        Skip health check wait
  --no-min-time-to-leader-slot Skip leader slot timing wait
  --skip-tower-sync            Skip tower file sync
  -y, --yes                    Skip confirmation prompts
  --to-peer <name|ip>          Auto-select peer (active node only)
```

### Global flags

```
  -c, --config <path>      Config file (default: ~/solana-validator-failover/solana-validator-failover.yaml)
  -l, --log-level <level>  Log level: debug, info, warn, error (default: info)
```

## Ports

| Port | Protocol | Purpose |
|------|----------|---------|
| 9898 | UDP | QUIC failover communication |

## Prerequisites

- Low-latency UDP route between validators (for QUIC)
- Passwordless SSH between nodes (for `switchover` command)
- Identity keypairs deployed on each node

## Hooks

Pre/post failover hooks with Go template interpolation. Available template fields:

| Field | Description |
|-------|-------------|
| `{{ .IsDryRunFailover }}` | bool: true if dry run |
| `{{ .ThisNodeRole }}` | "active" or "passive" |
| `{{ .ThisNodeName }}` | hostname of this node |
| `{{ .ThisNodePublicIP }}` | public IP of this node |
| `{{ .ThisNodeActiveIdentityPubkey }}` | active identity pubkey |
| `{{ .ThisNodePassiveIdentityPubkey }}` | passive identity pubkey |
| `{{ .ThisNodeClientVersion }}` | validator client version |
| `{{ .ThisNodeRPCAddress }}` | local RPC URL |
| `{{ .PeerNode* }}` | same fields for the peer node |

```yaml
    hooks:
      pre:
        when_active:
          - name: my-hook
            command: ./scripts/notify.sh
            args: ["--role={{ .ThisNodeRole }}"]
            must_succeed: true  # aborts failover on failure
            environment:
              PEER_IP: "{{ .PeerNodePublicIP }}"
      post:
        when_active:
          - name: notify-done
            command: ./scripts/notify.sh
            args: ["switchover-complete"]
```

## Development

```bash
make dev           # Docker dev environment with live-reload
make test          # Run tests
make build         # Build locally
make build-compose # Build via Docker (multi-arch)
```
