---
name: benchling-distributed-jobs
description: Guide for creating and running Benchling distributed jobs, including required attributes, example jobs, and local testing
---

# Benchling Distributed Jobs

Distributed jobs are used to process large datasets across multiple workers in parallel.

## Creating a Distributed Job

1. Subclass `AbstractSqlAlchemyJob` in a new file
2. Run `dev generate registration` to register the job

### Required Attributes

```python
from benchling.jobs.distributed_job import AbstractSqlAlchemyJob
from benchling.lib.core.eng_component import EngComponent
from benchling.lib.run_over_models_helpers.per_chunk_transaction_behavior import PerChunkTransactionBehavior

class MyDistributedJob(AbstractSqlAlchemyJob[PerChunkReturnType]):
    name = "unique_job_name"  # Unique identifier
    component = EngComponent.YOUR_TEAM  # Team for Sentry notifications
    transaction_behavior = PerChunkTransactionBehavior.COMMIT_PER_CHUNK  # Usually this
    use_tenant_context = True  # Usually True for tenant-scoped data (see below)
    update_modified_at = False  # Usually False for backfills
```

### Required Methods

```python
@classmethod
def get_model_cls(cls, model_cls_param: str | None = None) -> type[db.Model]:
    """Return the model class to iterate over."""
    return MyModel

@classmethod
def get_model_id_column(cls, model_cls: type[db.Model]) -> InstrumentedAttribute:
    """Return the ID column to traverse (must be indexed)."""
    return MyModel.id

@classmethod
def process_row(cls, model: db.Model, per_chunk_results: PerChunkReturnType) -> None:
    """Business logic for each row."""
    pass

@classmethod
def per_chunk_function(cls, models: list[db.Model], **kwargs) -> PerChunkReturnType:
    """Called once per chunk before process_row. Return value passed to process_row."""
    return None
```

### Optional Methods

- `get_job_specific_filters()` - SQLAlchemy filters to exclude rows
- `get_join_targets()` - Columns to join before filtering
- `get_worker_only_filters()` - Filters applied only in worker (after chunking)

## Tenant Handling: Two Independent Concepts

There are two separate tenant-related settings that are often confused:

### 1. `use_tenant_context` (DJ class attribute)

Controls whether tenant context is set before calling `process_row()`.

**When `use_tenant_context = True`:**
- Before processing each row, the framework resolves which tenant the model belongs to
- Sets `get_current_tenant()` to that tenant
- Your `process_row` code can use tenant-aware APIs, configs, and queries

**When `use_tenant_context = False`:**
- No tenant context is set (`get_current_tenant()` returns `None`)
- Use for tenantless models or when your logic doesn't need tenant context

**Requirement:** The model must have one of these mixins to use `use_tenant_context = True`:
- `HasTenant` or `JTIHasTenantMixin` with a non-null `tenant_id` column
- `CheckAuthMixin`
- `CheckAuthDerivedMixin`
- `HasTenantResolveable`
- `HasTenantTemporary` (with non-nullable tenant)

**Common Error:**
```
AssertionError: The Distributed Job Framework can not resolve a tenant from <class 'your.Model'>.
Please see the docs for a list of mixins which the DJ framework can use to resolve a tenant.
```

### 2. `include_tenants_string` (runtime parameter)

Controls which rows to process by filtering on `tenant_id` column.

**Requirement:** The model must have a **non-null `tenant_id` column** (different from the mixin requirement above).

**Key difference:** A model can have a `tenant_id` column but NOT support tenant context resolution. For example, the `Job` model has `tenant_id` (so tenant filtering works) but doesn't have the right mixins (so `use_tenant_context` must be `False`).

### Summary Table

| Model has...                     | `include_tenants_string` works? | `use_tenant_context = True` works? |
|----------------------------------|--------------------------------|-----------------------------------|
| `tenant_id` column only          | ✅ Yes                          | ❌ No                              |
| Tenant mixin (e.g., `HasTenant`) | ✅ Yes                          | ✅ Yes                             |
| Neither                          | ❌ No                           | ❌ No                              |

## Example Reference

See: `benchling/auth/permissions/migration/backfill_iam_resource_ancestry_job.py`

## Running Locally (dev-in-docker)

```bash
dev run script scripts.jobs.distributed_jobs.create_distributed_jobs --json-args '{
  "distributed_job_name": "your_job_name",
  "distributed_job_id": "unique-run-id",
  "slack_user_handle": "na",
  "include_tenants_string": "local"
}'
```

## Running on Adhoc

Use `manage.py run_script` with `--json-args` and `--json-script-env`. Note: This command can take ~60 seconds to start producing output.

```bash
python ./manage.py run_script scripts.jobs.distributed_jobs.create_distributed_jobs \
  --json-args '{"distributed_job_name": "your_job_name", "distributed_job_id": "unique-run-id", "slack_user_handle": "na", "include_tenants_string": "tenant_subdomain"}' \
  --json-script-env '{}'
```

**Important:** Do NOT use `--include-tenants` flag with this script - it will fail with "Include or exclude tenant filter is set but we're running across the entire deploy". Instead, pass tenant filtering via `include_tenants_string` in `--json-args`.

### Parameters

- `distributed_job_name` - The `name` attribute of your DJ class
- `distributed_job_id` - Unique ID for this run
- `slack_user_handle` - Slack handle for notifications (use "na" locally)
- `include_tenants_string` - Space-separated tenant subdomains to include (e.g., "tenant1 tenant2")
- `exclude_tenants_string` - Space-separated tenant subdomains to exclude
- `skip_archived_tenants` - Boolean to skip archived tenants
- `model_class_params` - Optional, if DJ supports multiple model classes

### Tenant Filtering Behavior

**How it works:**
- `include_tenants_string` filters tenants by subdomain (space-separated list)
- If `include_tenants_string` is empty (`""`), **all non-placeholder tenants** are included
- You cannot use both `include_tenants_string` and `exclude_tenants_string` together

**Important:** If a tenant subdomain doesn't exist, the allowlist will be empty and the job will complete immediately with no work done (the query becomes `WHERE tenant_id IN ()` which matches nothing).

**Common pitfall:** Using `"include_tenants_string": "local"` on an adhoc where no tenant has subdomain "local" results in zero rows processed and immediate COMPLETED status with `_last_id_scheduled = null`.

**To process all tenants:** Omit `include_tenants_string` or set it to `""`

**To find valid tenant subdomains:**
```sql
SELECT id, subdomain FROM tenant WHERE subdomain IS NOT NULL LIMIT 10;
```

### Tenant Filtering Requirements

- If using `include_tenants_string`, the model must have a `tenant_id` column
- Models without `tenant_id` (like `Tenant` itself) cannot use tenant filtering
- Set `use_tenant_context = False` for tenantless models

## Monitoring

Check worker logs for job execution:
```bash
dev compose logs worker 2>&1 | grep "distributed_job"
```

The job lifecycle:
1. `create_distributed_jobs` creates `DistributedJobMetadata` record
2. `launch_distributed_job_manager_task` picks up pending jobs
3. Manager spawns worker tasks for ID ranges
4. Workers call `process_row` for each model

## Distributed Job Metadata Table Schema

```sql
                                             Table "public.distributed_job_metadata"
       Column        |            Type             | Collation | Nullable |                       Default
---------------------+-----------------------------+-----------+----------+------------------------------------------------------
 id                  | bigint                      |           | not null | nextval('distributed_job_metadata_id_seq'::regclass)
 manager_job_id      | integer                     |           |          |
 job_size            | integer                     |           | not null |
 chunk_size          | integer                     |           | not null |
 max_chunk_size      | integer                     |           |          |
 _last_id_scheduled  | jsonb                       |           |          |
 distributed_job_id  | character varying(255)      |           | not null |
 finalizing_job_id   | integer                     |           |          |
 throttle_log        | jsonb                       |           | not null |
 parallelism         | integer                     |           | not null | 1
 job_status          | character varying(20)       |           |          |
 job_type            | character varying(11)       |           | not null | 'BACKFILL'::character varying
 job_params          | text                        |           |          |
 tenant_id_allowlist | integer[]                   |           |          |
 failure_reason      | character varying(255)      |           |          |
 api_identifier      | character varying(255)      |           | not null |
 tenant_id           | integer                     |           |          |
 modified_at         | timestamp without time zone |           | not null | now()
 created_at          | timestamp without time zone |           | not null | now()
```

Key columns:
- `distributed_job_id` - Unique identifier for this run (e.g., "my-job-001")
- `job_status` - Status: ACTIVE, COMPLETED, FAILED, etc.
- `job_params` - JSON containing distributed_job_name, slack_user_handle, model_class_param, etc.
- `_last_id_scheduled` - The last model ID that was scheduled for processing
- `job_size` - Number of IDs to process per worker task
- `chunk_size` - Number of rows per chunk within a worker
- `manager_job_id` - FK to job table for the manager job
- `tenant_id_allowlist` - Array of tenant IDs to filter processing
- `failure_reason` - Error message if job failed

### Useful Queries

```sql
-- Check status of a distributed job
SELECT id, distributed_job_id, job_status, job_params, _last_id_scheduled, failure_reason
FROM distributed_job_metadata
WHERE distributed_job_id = 'your-job-id';

-- Find recent distributed jobs
SELECT id, distributed_job_id, job_status, created_at
FROM distributed_job_metadata
ORDER BY created_at DESC LIMIT 10;

-- Find all job records for a distributed job run (workers are tagged with the distributed_job_id)
SELECT id, status, failure_reason, failure_text, parent_id, created_at
FROM job
WHERE 'your-distributed-job-id' = ANY(tags)
ORDER BY id;
```

## Throttling

Distributed jobs are throttled based on DB health. See `benchling/jobs/distributed_job_tasks/db_throttling.py`.

### Throttle Reasons

- `CPU_UTILIZATION` - DB CPU usage exceeds threshold
- `DB_STATUS` - RDS instance not in "available" state, or hasn't been available long enough

### DB_STATUS Throttling

When the RDS instance transitions to "available" status, jobs remain throttled for `DB_AVAILABLE_DELAY_MINUTES` (default: **6 hours**) to allow the DB to stabilize. This delay also triggers if the Redis cache is cold (e.g., new deploy, cache eviction).

### Redis Keys (for debugging)

Connect to Redis:
```bash
redis-cli --tls -h "$(echo $AURELIA_REDIS_URI | sed 's|rediss://||' | cut -d: -f1)"
```

Check throttle state:
```bash
GET /db/throttling/db_usage_throttled          # "1" = throttled, "0" = not
GET /db/throttling/db_usage_throttled_reason   # e.g., "DB_STATUS", "CPU_UTILIZATION"
HGETALL /db/db_throttling/db_last_available    # {db_id: timestamp} - when DB was last seen available
GET /db/throttling/last_refreshed_throttle_info # When throttle decision was last refreshed
GET /db/db_throttling/db_instance_identifier   # Cached RDS instance ID(s)
```

### Manually Unthrottling

If stuck in `DB_STATUS` throttle with an actually-available DB, backdate the `last_available` timestamp:
```bash
HSET /db/db_throttling/db_last_available <db-instance-id> "2026-01-01T00:00:00.000000"
```

Or clear throttle state directly (may get re-throttled on next check if delay hasn't passed):
```bash
SET /db/throttling/db_usage_throttled 0
DEL /db/throttling/db_usage_throttled_reason
```
