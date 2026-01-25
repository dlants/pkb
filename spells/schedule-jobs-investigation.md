Incident _incident_2026-01-20_-incdt-812_multiple-deploys-at-risk-of-running-out-of-redis

Declared at 2:29PM PT on Jan 20

Something was creating a large amount of redis keys.

Jan 20 at ~1pm, there's a backfill that triggers a big backlog of sync-to-warehouse jobs. At approximately
1:10PM PT, the max task duration for schedule_jobs jumps from ~1s to about 10-20s per run, and the average
number of queries per schedule_jobs jumps up from ~30 to ~1k-2k

The initial incident was resolved as expected behavior (redis keys generated for this sort of situation).
We're investigating the secondary effect of the schedule_jobs duration. Namely, we need to understand why
schedule_jobs took so long and generated this many queries during this time period, and see if we can mitigate
that.

[datadog link](https://app.datadoghq.com/dashboard/gzp-7rs-jbm/celery-and-jobs?fromUser=true&overlay=changes&overlayQuery=service%3Amonolith%20auto%3Atrue&refresh_mode=paused&tile_focus=3497933440606988&tpl_var_deploy%5B0%5D=mta-eu-central-1&tpl_var_task%5B0%5D=%2Awarehouse%2A&from_ts=1768937391627&to_ts=1768957886511&live=false)

schedule_jobs code is at `benchling/jobs/scheduling/schedule_jobs.py`

The affected deploy was mta-eu-central-1

[datadog APM dashboard for schedule_jobs
resource](https://app.datadoghq.com/apm/resource/monolith.task-worker/celery.run/1f182f3e81e27786?dependencyMap.showNetworkMetrics=false&env=e-eu-central-1&fromUser=false&groupMapByOperation=null&primaryTags=environment%3A%2A%2Cbnchstack%3A%2A&start=1768937340000&end=1768957860000&paused=true)

Found a trace of a run that took a really long time. The trace was very large! (>100MB).

[trace 696ff04d00000000ca060968dfff7505](https://app.datadoghq.com/apm/resource/monolith.task-worker/celery.run/1f182f3e81e27786?dependencyMap.showNetworkMetrics=false&env=e-eu-central-1&fromUser=false&graphType=waterfall&groupMapByOperation=null&primaryTags=environment%3A%2A%2Cbnchstack%3A%2A&shouldShowLegend=true&sp=%5B%7B%22mp%22%3A%7B%22size%22%3A%22lg%22%7D%2C%22p%22%3A%7B%7D%2C%22i%22%3A%22apm_trace-explorer-trace-panel%22%7D%5D&spanID=14532169090000830935&traceQuery=&start=1768937340000&end=1768957860000&paused=true#traces)

most of the time is in schedule_jobs.schedule (10s)
- schedule.select (3s)
  - Lots of queries like `SELECT job.id, job.label, job.parent_id, job.root_parent_id, job.status,
    job.failure_reason, job.failure_text, job.priority_level, job.timeout_seconds, job.timeout_at,
    job.started_at, job.active_at, job.retried_at, job.completed_at, job.failed_at, job.resolved_at,
    job.celery_task_name, job.retry_count, job.queue_after, job.tags, job.created_at, job.tenant_id FROM job
    WHERE job.status = ? AND ? = job.tenant_id AND ( now ( ) > job.queue_after OR job.queue_after IS ? ) AND
    job.celery_task_name IN ( ? ) AND job.priority_level > ? AND ( job.id NOT IN ( SELECT job.parent_id FROM
    job WHERE job.parent_id IS NOT ? AND job.status IN ( ? ) AND ? = job.tenant_id ) ) ORDER BY
    job.priority_level DESC, job.created_at ASC, job.id ASC LIMIT ?`

- schedule.start (7s)
  - lots of activity that looks like chunk_job_for_schema, `SELECT tenant.id, tenant.subdomain,
    tenant.global_identifier, tenant.name, tenant.display_name, tenant.archived, tenant.admin_data_policy_id,
    tenant.default_collaborator_data_policy_id, tenant.default_registry_collaborator_data_policy_id,
    tenant.avatar_url, tenant.avatar_url_48, tenant.avatar_url_128, tenant.modified_at FROM tenant WHERE
    tenant.id = ?`, `SELECT tenant.id, tenant.subdomain, tenant.global_identifier, tenant.name,
    tenant.display_name, tenant.archived, tenant.admin_data_policy_id,
    tenant.default_collaborator_data_policy_id, tenant.default_registry_collaborator_data_policy_id,
    tenant.avatar_url, tenant.avatar_url_48, tenant.avatar_url_128, tenant.modified_at FROM tenant WHERE
    tenant.id = ?`, BenchlingSession.commit, BenchlingSession.flush, UPDATE job SET status = ? timeout_at = ?
    started_at = ? WHERE job.id = ?, postgres.connection.commit / postgres.connection.rollback
  - lots of activity that looks like "task-producer sightglass3_graph_query", `SELECT tenant.id, tenant.subdomain, tenant.global_identifier, tenant.name, tenant.display_name, tenant.archived, tenant.admin_data_policy_id, tenant.default_collaborator_data_policy_id, tenant.default_registry_collaborator_data_policy_id, tenant.avatar_url, tenant.avatar_url_48, tenant.avatar_url_128, tenant.modified_at FROM tenant WHERE tenant.id = ?`
  - lots of src GET on redis `GET /obsync/px/1/schema_field_values_state/108474435`
  - lots of src GET on redis like `GET /obsync/sx/assay_result/4078423`
  - lots of src GET on redis like `GET /obsync/posted/250/entry/8460`

## Code investigation

### schedule_jobs call stack

```
benchling/jobs/scheduling/schedule_jobs.py
  schedule_jobs() task
    -> _process_jobs_for_queue()
       -> "schedule_jobs.schedule.select" span: queries Job table
       -> "schedule_jobs.schedule.start" span:
          -> benchling/jobs/scheduling/process_jobs.py::process_jobs()
             -> _process_one_job()
                -> _run_celery_task(task, tenant_id, args, kwargs)
                   -> task.apply_async_with_tenant_id()
                      -> benchling/taskq/context_tasks.py::apply_async_with_tenant_id()
                         -> self.apply_async() (standard Celery method)
```

### Where do GET /obsync redis operations come from?

The obsync redis keys are defined and accessed only in `benchling/cdc/queue_dedupe.py`:
- `BASE_NAMESPACE_WAREHOUSE_QUEUED_AT = "obsync/px"`
- `NAMESPACE_WAREHOUSE_SYNCED_AT = "obsync/sx"`
- `BASE_NAMESPACE_OBJECT_SYNC_POSTED = "obsync/posted"`

`queue_dedupe.processed_since_queue()` reads these keys. It's called from:
- `benchling/warehouse/object_sync/sync_rows_to_warehouse.py:518` - during warehouse sync execution
- `benchling/warehouse/object_sync/queueing/create_object_sync_jobs.py:362,385` - during object sync job creation
- `benchling/inventory/storables/storable_session_cache_handlers.py:46` - inventory validation
- `benchling/search/indexer/deduping.py:18` - search indexer deduplication

### Resolution: Distributed tracing artifact

The trace shows redis GETs on `/obsync/...` keys happening during `schedule_jobs.schedule.start` span.
However, the code path from `schedule_jobs` -> `process_jobs` -> `_run_celery_task` -> `apply_async`
doesn't appear to call any of the above `queue_dedupe` call sites.

**Root cause**: This is a distributed tracing artifact, not actual synchronous execution.

When `schedule_jobs` enqueues a task via `apply_async`, Celery's distributed tracing propagates the trace ID
to the child task. When that child task executes (potentially on a different worker), its spans are associated
with the same trace ID. In Datadog's trace view, these child task spans appear nested under the parent
`schedule_jobs` trace, making it look like the redis operations are happening synchronously within
`schedule_jobs`.

In reality:
1. `schedule_jobs` enqueues tasks (fast, just sends messages to broker)
2. Child tasks execute asynchronously on workers (this is where obsync redis reads happen)
3. Datadog displays all spans with the same trace ID together, creating the illusion of synchronous execution

This explains why the trace was >100MB - it included spans from many child tasks that were linked via
distributed tracing.

### Remaining question

If the obsync redis operations are happening in child tasks (not synchronously in `schedule_jobs`), then
what is actually causing `schedule_jobs` itself to take 10-20s during the incident period?

Looking at the trace more carefully, the operations that are **actually synchronous** in `schedule_jobs`:
- `schedule.select` (3s): Job table queries to find jobs to schedule
- `schedule.start` (7s): For each job - tenant lookup, UPDATE job status, enqueue via apply_async

The 7s in `schedule.start` is likely due to:
- Large number of jobs being scheduled (backfill created many jobs)
- Each job requires: tenant lookup query + UPDATE job status + commit
- This adds up when scheduling hundreds/thousands of jobs

## Trace analysis

Used `notes/analyze_trace.py` to extract and analyze schedule_jobs spans from traces.

### Slow trace breakdown

```
$ python notes/analyze_trace.py /tmp/trace

Loading trace from /tmp/trace...
Truncation: EXCEEDED_MAX_STATE_SIZE
Total spans: 84202
schedule_jobs spans: 5802

=== Span tree (schedule_jobs only) ===
schedule_jobs.schedule.start [x2] (6.668s)
  [query] UPDATE job SET status = ?... (n=28, 0.337s)
  [query] SELECT job.id, job.label...  (n=28, 0.302s)
  [query] SELECT tenant.id...          (n=307, 0.258s)
  [query] SELECT job.parent_id...      (n=28, 0.028s)
schedule_jobs.schedule.select [x2] (3.920s)
  [query] SELECT job.id, job.label...  (n=914, 1.104s)  ← Main bottleneck
  [query] SELECT tenant.id...          (n=255, 0.218s)
  [query] SELECT count (*)...          (n=2, 0.002s)
schedule_jobs.get_pending_stats (0.769s)
  [query] SELECT tenant.id...          (n=5, 0.340s)
  [query] SELECT job.celery_task_name, job.priority_level, count... (n=2, 0.024s)
schedule_jobs.initialize_throttlers (0.019s)

=== Query summary ===
Query (truncated)                                                   Count    Total (s)
---------------------------------------------------------------------------------------
SELECT job.id, job.label, job.parent_id, job.root_parent_id...       1390        1.953
SELECT tenant.id, tenant.subdomain, tenant.global_identifier...       863        1.065
UPDATE job SET status = ? timeout_at = ? started_at = ?...             53        0.644
```

### Fast trace breakdown

```
$ python notes/analyze_trace.py /tmp/trace-small

Loading trace from /tmp/trace-small...
Truncation: NO_TRUNCATION
Total spans: 1396
schedule_jobs spans: 211

=== Span tree (schedule_jobs only) ===
celery.run [schedule_jobs] (0.245s)
  schedule_jobs.schedule (0.134s)
    schedule_jobs.schedule.start x4 (0.061s)
      [query] UPDATE job SET status = ?... (n=3, 0.011s)
      [query] SELECT job.id, job.label...  (n=3, 0.004s)
      [query] SELECT job.parent_id...      (n=3, 0.004s)
    schedule_jobs.schedule.select x4 (0.045s)
      [query] SELECT count (*)...          (n=4, 0.007s)
      [query] SELECT tenant.id...          (n=6, 0.007s)
      [query] SELECT job.id, job.label...  (n=3, 0.005s)
    schedule_jobs.initialize_throttlers (0.010s)
  schedule_jobs.get_pending_stats (0.101s)
    [query] SELECT tenant.id...            (n=5, 0.023s)
    [query] SELECT count (*)...            (n=4, 0.005s)
```

### Comparison summary

| Metric | Slow trace | Fast trace | Ratio |
|--------|-----------|------------|-------|
| **Total duration** | ~11s | 0.25s | 44x |
| **schedule.select duration** | 3.92s | 0.045s | 87x |
| **schedule.start duration** | 6.67s | 0.061s | 109x |
| **Job SELECT queries** | 914 | 3 | 305x |
| **Tenant SELECT queries** | 563 | 11 | 51x |
| **UPDATE job queries** | 28 | 3 | 9x |
| **Tasks enqueued** | 835 | 9 | 93x |

**Root cause:** The slow trace executed **914 job selection queries** in `schedule.select` (1.1s), compared
to just 3 in the fast trace. This query finds pending jobs for each task type. During the incident, the
backfill created a large backlog of jobs across many task types, causing `schedule_jobs` to iterate through
far more task types and execute many more queries.

The `schedule.start` phase was also slow (6.67s) because it had to:
- Start 835 tasks (vs 9 normally)
- Execute 307 tenant lookups
- Perform 28 job UPDATEs with commits

### schedule.start deep dive

Drilling into the two `schedule.start` spans (3.25s and 3.41s each), there's significant unaccounted time:

| Span | Duration | Child spans | Unaccounted | % |
|------|----------|-------------|-------------|---|
| start #1 | 3.255s | 1.854s | 1.401s | 43% |
| start #2 | 3.413s | 1.553s | 1.860s | 55% |

**Direct children breakdown for span #1 (3.255s):**

| Span type | Count | Total |
|-----------|-------|-------|
| celery.apply | 207 | 0.626s |
| all (SqlAlchemy) | 28 | 0.572s |
| BenchlingSession.commit | 14 | 0.504s |
| postgres.query | 182 | 0.152s |

**Direct children breakdown for span #2 (3.413s):**

| Span type | Count | Total |
|-----------|-------|-------|
| BenchlingSession.commit | 14 | 0.678s |
| celery.apply | 222 | 0.517s |
| all (SqlAlchemy) | 28 | 0.252s |
| postgres.query | 125 | 0.106s |

**Gap analysis:**

Both spans have large gaps (uninstrumented time) at roughly the same relative offset (~1.3s into the span):

- **Span #1**: 0.748s gap at offset 1.293s (between postgres.query spans)
- **Span #2**: 1.245s gap at offset 1.265s (between celery.apply spans)

**Timeline around gap in span #1:**
```
[ 1.279s] postgres.query (0.001s)
[ 1.282s] celery.apply (0.003s)
[ 1.287s] postgres.query (0.001s)
[ 1.289s] celery.apply (0.002s)
[ 1.292s] postgres.query (0.001s)
[ 1.293s] *** GAP: 0.748s ***
[ 2.041s] postgres.query (0.001s)
[ 2.044s] celery.apply (0.004s)
[ 2.049s] postgres.query (0.001s)
[ 2.052s] celery.apply (0.003s)
[ 2.056s] postgres.query (0.001s)
```

**Timeline around gap in span #2:**
```
[ 1.250s] postgres.query (0.001s)
[ 1.253s] celery.apply (0.002s)
[ 1.256s] celery.apply (0.003s)
[ 1.261s] postgres.query (0.001s)
[ 1.263s] celery.apply (0.002s)
[ 1.265s] *** GAP: 1.245s ***
[ 2.511s] celery.apply (0.005s)
[ 2.517s] postgres.query (0.001s)
[ 2.520s] celery.apply (0.002s)
[ 2.524s] celery.apply (0.003s)
[ 2.528s] postgres.query (0.001s)
```

The pattern before and after the gap is identical: interleaved `celery.apply` and `postgres.query` spans
at ~3ms intervals. The gap occurs in the middle of normal job processing, not at a batch boundary or
commit point.

### Second slow trace (trace-875e) breakdown

This trace was truncated (`EXCEEDED_MAX_STATE_SIZE`) and missing the root `celery.run` span.

| Span | Duration |
|------|----------|
| schedule.select | 2.24s |
| schedule.start | 3.04s |
| get_pending_stats | 0.63s |
| initialize_throttlers | 0.01s |
| **Total (instrumented)** | ~6s |

| Metric | Value |
|--------|-------|
| Total spans | 54,695 |
| schedule_jobs spans | 2,533 |
| Job SELECT queries | 438 |
| Tenant SELECT queries | 385 |
| UPDATE job queries | 31 |
| celery.apply spans | 484 |

**Comparison to first slow trace:**

| Metric | Trace 1 (~11s) | Trace 2 (~6s) | Ratio |
|--------|----------------|---------------|-------|
| schedule.select | 3.92s | 2.24s | 1.75x |
| schedule.start | 6.67s | 3.04s | 2.2x |
| Job SELECT queries | 914 | 438 | 2.1x |
| Tasks enqueued | 835 | 484 | 1.7x |

The second trace shows the same pattern but at lower scale - fewer jobs in the backlog resulted in fewer queries and faster execution. Both traces confirm the linear relationship between job backlog size and `schedule_jobs` duration.

## Hot Spots Summary

### 1. `schedule.select` - Slow Query (914 queries, 1.1s)

**Location:** `pending_job_queries.py` → `query_next_job_page()` (lines 118-144)

**Root cause:** For each tenant with pending jobs, `_select_jobs_for_tenant()` calls `query_next_job_page()` in a **pagination loop**. Each call executes **2 queries** (one for `CREATED`, one for `RETRIED` status):

```python
created_jobs = _get_ordered_jobs_query(..., status=JobStatus.CREATED, ...).all()
retried_jobs = _get_ordered_jobs_query(..., status=JobStatus.RETRIED, ...).all()
```

During the incident with many tenants having backlogs, this multiplied: `914 queries ≈ 2 queries × ~450 page fetches` across many tenants.

**The expensive query** is in `base_get_pending_jobs_query()` (line 37-70) which includes a **subquery to filter blocked parent jobs**:
```sql
~Job.id.in_(blocked_parent_jobs_ids_subquery)
```

This subquery scans for child jobs in `IN_FLIGHT_STATUSES + PENDING_STATUSES` to exclude their parents.

---

### 2. `schedule.start` - Where Time Goes (6.67s)

**Location:** `schedule_jobs.py` → `_process_jobs_for_queue()` (lines 48-72) and `process_jobs.py`

**Breakdown per chunk of 16 jobs:**

| Operation | Location | Notes |
|-----------|----------|-------|
| `Job.query...with_for_update()` | `process_jobs.py:119-124` | SELECT with row lock + deferred celery_args |
| `Job.parent_id` children query | `process_jobs.py:127-132` | Gets child statuses |
| `task.apply_async_with_tenant_id()` | `process_jobs.py:20-24` | Enqueues to Celery (307 tenant lookups happen here) |
| `db.session.commit()` | `schedule_jobs.py:69` | Commits job status UPDATE |

**The 307 tenant SELECT queries** come from `apply_async_with_tenant_id()` which likely loads the tenant object for each job being enqueued.
