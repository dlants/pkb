---
name: benchling-jobs
description: Guide for Benchling Jobs framework including Celery tasks, the Job model, and how jobs are scheduled and executed
---

# Benchling Jobs Framework

The Jobs framework is an abstraction on top of Celery for tracking and managing background tasks.

## Architecture Overview

1. **Job Model** (`benchling/jobs/models/job.py`) - Database record tracking task state
2. **Celery Tasks** - Actual task execution via Celery workers
3. **schedule_jobs** - Beat task that picks up CREATED jobs and dispatches them to Celery

## Job Lifecycle

```
CREATED → SCHEDULED → STARTED → ACTIVE → COMPLETED
                                      ↘ FAILED → RESOLVED
                                      ↘ RETRIED → (re-enters queue)
```

## Job Table Schema

```sql
                                           Table "public.job"
      Column      |            Type             | Collation | Nullable |             Default             
------------------+-----------------------------+-----------+----------+---------------------------------
 id               | integer                     |           | not null | nextval('job_id_seq'::regclass)
 label            | character varying           |           | not null | 
 parent_id        | integer                     |           |          | 
 root_parent_id   | integer                     |           |          | 
 status           | character varying(9)        |           | not null | 'CREATED'::character varying
 failure_reason   | character varying(13)       |           |          | 
 failure_text     | text                        |           |          | 
 priority_level   | integer                     |           | not null | 200
 timeout_seconds  | integer                     |           | not null | 
 timeout_at       | timestamp without time zone |           |          | 
 started_at       | timestamp without time zone |           |          | 
 active_at        | timestamp without time zone |           |          | 
 retried_at       | timestamp without time zone |           |          | 
 completed_at     | timestamp without time zone |           |          | 
 failed_at        | timestamp without time zone |           |          | 
 resolved_at      | timestamp without time zone |           |          | 
 celery_task_name | character varying(1000)     |           |          | 
 celery_args      | jsonb                       |           | not null | 
 celery_kwargs    | jsonb                       |           | not null | 
 retry_count      | integer                     |           | not null | 0
 queue_after      | timestamp without time zone |           |          | 
 tags             | character varying(255)[]    |           |          | 
 created_at       | timestamp without time zone |           | not null | now()
 tenant_id        | integer                     |           |          | 
```

Key columns:
- `label` - Human-readable label (e.g., "celery:task_name")
- `status` - Job status (CREATED, STARTED, ACTIVE, RETRIED, COMPLETED, FAILED, RESOLVED)
- `celery_task_name` - The Celery task to execute
- `celery_kwargs` - JSON kwargs passed to the task
- `tags` - Array of string tags for grouping/filtering
- `failure_reason` - EXCEPTION, TIMEOUT, or CHILD_FAILURE
- `failure_text` - Error details
- `tenant_id` - Optional tenant association

## Creating Jobs

Use `create_celery_job` to create a Job record:

```python
from benchling.jobs.job_task import create_celery_job

job = create_celery_job(
    my_task_function,
    kwargs={"param": "value"},
    tenant=tenant,  # optional
)
job.tags = ["my_tag"]
db.session.add(job)
db.session.commit()
```

## Job Execution Flow

1. `schedule_jobs` beat task runs every ~5 seconds
2. Queries for jobs with status CREATED or RETRIED
3. Dispatches matching jobs to Celery via `apply_async`
4. Worker picks up task and executes it
5. Job status updated based on result

## Debugging Jobs

### Find jobs by task name
```sql
SELECT id, status, failure_reason, failure_text, created_at 
FROM job 
WHERE celery_task_name = 'distributed_job_worker'
ORDER BY created_at DESC LIMIT 10;
```

### Find jobs by tag
```sql
SELECT id, status, celery_kwargs 
FROM job 
WHERE 'my-tag' = ANY(tags);
```

### Find jobs for a distributed job run
Distributed job workers are tagged with the `distributed_job_id`:
```sql
SELECT id, status, failure_reason, failure_text
FROM job
WHERE 'your-distributed-job-id' = ANY(tags)
ORDER BY id;
```

### Check for stuck jobs
```sql
SELECT id, status, started_at, celery_task_name 
FROM job 
WHERE status IN ('CREATED', 'STARTED', 'ACTIVE') 
AND created_at < NOW() - INTERVAL '1 hour';
```

## Worker Logs

View task execution in worker logs:
```bash
dev compose logs worker 2>&1 | grep "task_name"
```

Log format shows:
- `Task <name>[<id>] received` - Task picked up by worker
- `Task <name>[<id>] succeeded in Xs` - Task completed
- `Task <name>[<id>] failed` - Task failed (check for exception in surrounding logs)

## Distributed Jobs

For large-scale data processing, see the `benchling-distributed-jobs` skill. Distributed jobs use this Jobs framework under the hood but add:
- Automatic chunking and parallelism
- Progress tracking via `distributed_job_metadata`
- Throttling based on database load
