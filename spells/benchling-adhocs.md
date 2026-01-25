---
name: benchling-adhoc
description: Drive Benchling adhoc SSM sessions with a scriptable DSL for running bash commands, SQL queries, and Redis commands on remote environments
---

# Benchling Adhoc Session Driver

This skill allows you to run commands on Benchling adhoc instances via AWS SSM sessions using a simple scripting DSL.

## Prerequisites

1. An active adhoc session requested through Benchling's adhoc system
2. The instance ID (e.g., `i-0734b70988626100b`)
3. AWS credentials configured with the `dev.adhoc-connect` profile

## Quick Start

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts <instance-id> <<'EOF'
<dsl-script>
EOF
```

## DSL Commands

### `send <command>`

Sends a command to the session. Append `&& echo "--END--"` to create a marker for completion detection.

### `send <<DELIMITER` (heredoc)

Sends multiple lines to the session. All lines until `DELIMITER` appears on its own line are sent.

```
send <<SQL
SELECT *
FROM users
WHERE active = true;
SQL
```

### `waitfor <pattern>`

Waits until the specified pattern appears in the output. Use with markers to know when a command completes.

### `mark <name>`

Records the current line number in the output. Use to bookmark positions for later extraction.

### `idle <ms>`

Waits for the specified milliseconds. Useful for letting output settle.

### `output <start> [end]`

Extracts output between two marks. If `end` is omitted, extracts from `start` to the current position.
This captured output will be returned when the script completes.

## Examples

### Run a bash command

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-0734b70988626100b <<'EOF'
mark cmd_start
send ls -la /src ; echo "--DONE--"
waitfor --DONE--
mark cmd_end
output cmd_start cmd_end
EOF
```

### Run a SQL query

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-0734b70988626100b <<'EOF'
mark q1_start
send DB_URI=$(echo $AURELIA_SQLALCHEMY_DATABASE_URI | sed 's/postgresql+psycopg2/postgresql/') && psql -c "SELECT count(*) FROM registry" "$DB_URI" ; echo "--DONE--"
waitfor --DONE--
mark q1_end
output q1_start q1_end
EOF
```

### Multiple commands with setup

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-0734b70988626100b <<'EOF'
send cd /src ; echo "--SETUP--"
waitfor --SETUP--

mark query_start
send grep -r "SomePattern" . ; echo "--DONE--"
waitfor --DONE--
mark query_end

output query_start query_end
EOF
```

## Patterns

### Bash Commands

Use `; echo "--END--"` to mark command completion (use `;` not `&&` so the marker prints even if the command fails):

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-xxx <<'EOF'
mark start
send ls -la /src ; echo "--END--"
waitfor --END--
mark end
output start end
EOF
```

### PostgreSQL Queries

Use `psql` with the `-c` flag for non-interactive SQL. Convert the SQLAlchemy URI to standard postgres format.

**Always use `PAGER=cat`** to disable psql's pager. Without this, psql may open `less` which blocks indefinitely waiting for user input, causing timeouts.

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-xxx <<'EOF'
mark start
send DB_URI=$(echo $AURELIA_SQLALCHEMY_DATABASE_URI | sed 's/postgresql+psycopg2/postgresql/') && PAGER=cat psql -c "SELECT count(*) FROM registry" "$DB_URI" ; echo "--END--"
waitfor --END--
mark end
output start end
EOF
```

Multi-line SQL queries using psql heredoc:

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-xxx <<'EOF'
mark start
send DB_URI=$(echo $AURELIA_SQLALCHEMY_DATABASE_URI | sed 's/postgresql+psycopg2/postgresql/')
send PAGER=cat psql "$DB_URI" << 'SQL' ; echo "--END--"
send SELECT id, name, created_at
send FROM registry
send WHERE created_at > '2024-01-01'
send LIMIT 10;
send SQL
waitfor --END--
mark end
output start end
EOF
```

**Important:** Before writing SQL queries, read the relevant skill files to understand table schemas:

- For `job` table: read the `benchling-jobs` skill
- For `distributed_job_metadata` table: read the `benchling-distributed-jobs` skill

This ensures you use the correct column names (e.g., `failed_at` not `modified_at` for the job table).

### Redis Commands

Use non-interactive redis-cli commands (interactive mode has issues with pubsub notifications):

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-xxx <<'EOF'
mark start
send REDIS_HOST=$(echo $AURELIA_REDIS_URI | sed -E 's|rediss?://([^:/]+).*|\1|') && redis-cli -h $REDIS_HOST -p 6379 --tls PING ; echo "--END--"
waitfor --END--
mark end
output start end
EOF
```

Multiple Redis commands:

```bash
~/.claude/skills/benchling-adhoc/scripts/driver.ts i-xxx <<'EOF'
mark start
send REDIS_HOST=$(echo $AURELIA_REDIS_URI | sed -E 's|rediss?://([^:/]+).*|\1|')
send redis-cli -h $REDIS_HOST -p 6379 --tls GET some:key:name ; echo "--END--"
waitfor --END--
mark end
output start end
EOF
```

## Debugging

Set `DEBUG=1` to see raw PTY output:

```bash
DEBUG=1 ~/.claude/skills/benchling-adhoc/scripts/driver.ts i-xxx bash <<'EOF'
...
EOF
```

## Notes

- Output is automatically cleaned of ANSI codes and terminal noise
- The session runs inside a docker container with all necessary env vars pre-configured
- Sessions are logged to S3 for audit purposes
- Default waitfor timeout is 5 seconds; increase `idle` if needed for slow queries

## Important: Adhoc vs. Production Environment

The adhoc instance is **not the same server** as the web/worker containers that run production code. This means:

- **Environment variables** on the adhoc may differ from those in web/worker containers
- **Local files** (e.g., `/opt/aurelia/cache/config.json`) may not exist or may have different contents
- **Config values** that are set via env vars or S3 in production may not be present on adhoc

To investigate production config values, use:

1. **Database queries** - Query `config_override` table for DB-stored configs
2. **Code inspection** - Check `config/deploy_config/base.py` for Python defaults
3. **The configs skill** - See the `benchling-configs` skill for the full config resolution order

The adhoc is primarily useful for:

- Running SQL queries against the production database
- Running Redis commands against the production Redis
- Checking deploy-level metadata (DEPLOY_NAME, DEPLOY_ENVIRONMENT)
- Running one-off scripts that need database access
