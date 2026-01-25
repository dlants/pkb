---
name: benchling-dev-docker
description: Guide for Benchling dev-in-docker local development environment, including docker compose services, common commands, and troubleshooting
---

# Benchling Dev-in-Docker Environment

This skill documents the local development environment for the Benchling monolith.

## Required Services

The local dev environment uses Docker Compose with these key services:

- **db** - PostgreSQL database
- **redis** - Redis for Celery task queue
- **web** - Flask web application
- **worker** - Celery task worker
- **es01** - Elasticsearch/OpenSearch for search
- **es-test** - Test Elasticsearch instance

## Common Commands

### Check running services
```bash
dev compose ps
```

### Start required services
```bash
dev compose up -d db redis  # Start database and redis
dev compose up -d           # Start all services
```

### View logs
```bash
dev compose logs worker              # All worker logs
dev compose logs worker -f           # Follow worker logs
dev compose logs worker 2>&1 | grep "pattern"  # Filter logs
```

### Update dependencies after switching branches
```bash
dev setup backend-deps        # Install Python requirements
dev setup backend-deps --force  # Force reinstall
```

### Generate code registration (after adding new registered classes)
```bash
dev generate registration
```

## Database Queries

Use `dev db psql` to run SQL queries against the local PostgreSQL database:

```bash
# Run a query directly
dev db psql -c "SELECT * FROM tenant LIMIT 5;"

# Describe a table schema
dev db psql -c "\d job"

# List tables matching a pattern
dev db psql -c "\dt distributed_job*"

# Interactive psql session
dev db psql
```

### Useful Tables

- `tenant` - Tenant information (id, subdomain, name)
- `job` - Celery job records with status tracking
- `distributed_job_metadata` - Distributed job state and progress

### Common Queries

```sql
-- Check recent jobs
SELECT id, label, status, celery_task_name, created_at
FROM job ORDER BY created_at DESC LIMIT 10;

-- Find jobs by task name
SELECT id, status, failure_reason, failure_text
FROM job WHERE celery_task_name ILIKE '%pattern%';

-- Check distributed job status
SELECT id, distributed_job_id, job_status, _last_id_scheduled
FROM distributed_job_metadata ORDER BY created_at DESC;
```

## Networking

The dev container and docker-compose services share a bridge network (`aurelia_default`). Services can reach each other by container name (e.g., `http://temporal:8233`).

**Important**: Port mappings in `docker-compose.yml` expose ports to the dev container, NOT to your host machine. To access a service from your host browser, you need to add the port to the dev-in-docker's own port forwarding configuration (separate from the monolith's docker-compose.yml).

From inside the dev container, you can reach services via:
- `localhost:<port>` (if port is mapped in docker-compose.yml)
- `<service-name>:<port>` (e.g., `temporal:8233`)

## Troubleshooting

### "Cannot connect to redis" errors
Start the redis service:
```bash
dev compose up -d redis
```

### "could not translate host name 'db'" errors
Start the database service:
```bash
dev compose up -d db
```

### ImportError for missing module attributes
Dependencies may be out of sync. Check `requirements.txt` for expected version and run:
```bash
dev setup backend-deps
```

## Running Scripts

Scripts are run via the `dev run script` command:
```bash
dev run script <script.module.path> --json-args '{...}'
```

Example:
```bash
dev run script scripts.jobs.distributed_jobs.create_distributed_jobs --json-args '{
  "distributed_job_name": "my_job",
  "distributed_job_id": "unique-id",
  "slack_user_handle": "na"
}'
```
