# Object Sync System Overview

The warehouse sync system keeps Benchling's analytics warehouse in sync with the primary database. This document explains how change events are generated, propagated, and deduplicated.

## High-Level Architecture

```
1. CHANGE DETECTION (Postgres Triggers)
   ─────────────────────────────────────
   INSERT/UPDATE/DELETE on tracked tables
            │
            ▼
   record_cdc_*_event() triggers
            │
            ▼
   CDCEventLog table (one row per change, per event_processor)

2. QUEUEING (Celery Tasks)
   ───────────────────────
   object_sync_queue_tenantless (beat task, runs every few seconds)
            │
            ▼
   object_sync_queue_for_tenant (per-tenant task)
            │
            ▼
   queue_records() - reads CDCEventLog, calls process_logs()
            │
            ▼
   Creates queue_object_sync_jobs Job records

3. JOB EXECUTION
   ─────────────────
   schedule_jobs picks up queue_object_sync_jobs
            │
            ▼
   create_object_sync_jobs.py - amplifies changes, creates object_sync_job
            │
            ▼
   sync_rows_to_warehouse.py - actually syncs to warehouse

```

## 1. Change Detection: CDC Triggers

**Source**: `benchling/cdc/trigger.py`, `benchling/cdc/models.py`

Postgres triggers fire on INSERT/UPDATE/DELETE for tracked tables and insert rows into `cdc_event_log`:

```sql
-- Example: After INSERT on container table
INSERT INTO cdc_event_log (table_name, row_id, event_source, tenant_id, event_processor, ...)
VALUES ('container', 12345, 'INSERT', 42, 'OBJECT_SYNC', ...)
```

Key points:
- **Fan-out**: Each change creates multiple `CDCEventLog` rows - one per enabled `EventProcessor` (currently 3: OBJECT_SYNC, SIGHTGLASS3_ASYNCHRONOUS_INDEXING, SIGHTGLASS3_SYNCHRONOUS_INDEXING)
- **CHILD_DELETE events**: When a row is deleted or its FK changes, triggers also create events for the parent row (e.g., deleting a Container creates an event for its parent Plate)
- **modified_columns**: Tracks which columns changed, enabling downstream filtering

### Performance Impact

**Triggers run synchronously within the same transaction as the original change.** When you `INSERT INTO container`:

1. Postgres inserts your row
2. **Before commit**, the `AFTER INSERT` trigger fires
3. Trigger converts row to JSONB, computes modified_columns, inserts 3 rows into `cdc_event_log`
4. Only then does your transaction commit (or rollback, which also rolls back CDC rows)

Every tracked INSERT/UPDATE/DELETE pays this cost:
- JSONB serialization of the row (`to_jsonb(new_table)`)
- Function calls for `modified_columns()`, `tenant_id_from_row()`, etc.
- 3 INSERT statements into `cdc_event_log` (one per enabled EventProcessor)
- For DELETEs/UPDATEs: additional queries to find FK relationships for CHILD_DELETE events

The trigger code uses `MATERIALIZED` CTEs to minimize this overhead since it's on the hot path for every write.

### Future: WAL-based CDC

Postgres has a **Write-Ahead Log (WAL)** that records all changes. Tools like **Debezium** can read the WAL and stream changes without trigger overhead. Benchling has Debezium infrastructure (`DebeziumSignal`, `DebeziumHeartbeat` models) and `*_WAL_SHADOW` event processors that appear to be a WIP migration from trigger-based to WAL-based CDC.

| Approach | Pros | Cons |
|----------|------|------|
| Triggers | Simple, works with any Postgres, easy tenant context | Write amplification (3x), adds latency to every transaction |
| WAL/Debezium | No write amplification, doesn't slow transactions | More infrastructure (Kafka), harder to get tenant context |

### CDCEventLog Model

```python
class CDCEventLog:
    table_name: str          # e.g., "container"
    row_id: int              # PK of the changed row
    event_source: EventSource  # INSERT, UPDATE, DELETE, CHILD_DELETE
    event_processor: EventProcessor  # OBJECT_SYNC, SIGHTGLASS3_*, etc.
    tenant_id: int
    modified_columns: list[str]  # Which columns changed
    old_row_values: dict     # Previous row state (for UPDATE/DELETE)
    txid: int                # Postgres transaction ID
```

## 2. CDC Event Processing: object_sync_queue

**Source**: `benchling/warehouse/object_sync/queueing/tasks.py`

A beat task (`object_sync_queue_tenantless`) runs periodically and:
1. Finds tenants with pending `CDCEventLog` records for `EventProcessor.OBJECT_SYNC`
2. Enqueues `object_sync_queue_for_tenant` for each tenant

The per-tenant task calls `queue_records()` which:
1. Reads `CDCEventLog` records in batches
2. Calls `process_logs()` to create `Job` records
3. Deletes the processed `CDCEventLog` records

### process_logs() Flow

```python
def process_logs(records):
    # Filter to only object sync trigger tables
    sync_trigger_records = filter_sync_trigger_records(records, trigger_tables)

    # Group by priority (based on txid size and table type)
    for job_priority_info, group_records in ...:
        for chunk in chunk_iterator(group_records, batch_size):
            # Convert CDCEventLog -> WarehouseSyncUnit
            unit_dicts = [WarehouseSyncUnit.from_event_record(r).to_dict() for r in chunk]

            # Create a Job that will run queue_object_sync_jobs
            job = create_variable_priority_celery_job(
                queue_object_sync_jobs,
                args=[unit_dicts],
                ...
            )
```

## 3. Job Execution: Amplification & Sync

### queue_object_sync_jobs

**Source**: `benchling/warehouse/object_sync/queueing/jobs.py`

This job takes `WarehouseSyncUnit` items and:
1. **Pre-amplification dedupe**: Uses obsync Redis keys to skip items already queued
2. **Amplification**: Expands changes to include dependent rows (see below)
3. **Post-amplification dedupe**: Deduplicates the expanded set
4. Creates `object_sync_job` Job records for actual sync

### What is Amplification?

The warehouse is **denormalized** - it includes data from related tables inline to make analytics queries easier (no JOINs needed).

**Example**: The `container$raw` warehouse table includes:
- Container's own fields (name, volume, etc.)
- `creator_id` → denormalized User fields (creator name, email)
- `plate_id` → denormalized Plate fields

When a **User** is updated (e.g., name change), all **Containers created by that user** must be re-synced to update their denormalized creator data.

**Amplification** finds all these dependent rows:

```python
# In batch_create_object_sync_jobs.py
def _batch_get_affected_items(sync_units):
    # For each sync_unit, find dependent rows that need re-sync
    # e.g., User change → find all Containers with that creator_id
```

### should_amplify.py

Determines whether a change needs amplification based on:
- Which columns changed (`modified_columns`)
- Whether those columns are used in denormalized warehouse columns
- Table-specific rules (some changes don't affect dependents)

## 4. Deduplication: The obsync Redis Keys

**Source**: `benchling/cdc/queue_dedupe.py`

Redis keys prevent duplicate work across the pipeline:

### Redis Namespaces

| Namespace | Purpose | Example Key |
|-----------|---------|-------------|
| `obsync/px/0` | Pre-amplification queue tracking | `obsync/px/0/container/12345` |
| `obsync/px/1` | Post-amplification queue tracking | `obsync/px/1/container/12345` |
| `obsync/sx` | Synced-at tracking | `obsync/sx/container/12345` |
| `obsync/posted/<priority>` | Job posted tracking | `obsync/posted/250/container/12345` |

### Key Functions

```python
def processed_since_queue(table_name: str, row_ids: list[int], key_base: str) -> dict[int, bool]:
    """Check if items were already processed since being queued."""
    # Returns {row_id: True/False} for each row

def mark_queued(table_name: str, row_ids: list[int], key_base: str):
    """Mark items as queued (sets Redis keys with timestamps)."""

def mark_synced(table_name: str, row_ids: list[int]):
    """Mark items as synced to warehouse."""
```

### Deduplication Flow

```
CDCEventLog created for Container 12345
       │
       ▼
queue_object_sync_jobs receives WarehouseSyncUnit
       │
       ▼
Pre-amplification dedupe: Check obsync/px/0/container/12345
  - If exists and timestamp > queue time → skip (already being processed)
  - Otherwise → continue, mark as queued
       │
       ▼
Amplification: Find dependent rows (e.g., Container's Plate needs re-sync)
       │
       ▼
Post-amplification dedupe: Check obsync/px/1/... for all affected items
       │
       ▼
Create object_sync_job for items that pass dedupe
       │
       ▼
sync_rows_to_warehouse executes
       │
       ▼
Mark synced: Set obsync/sx/container/12345 timestamp
```

## Source Files Reference

| File | Purpose |
|------|---------|
| `benchling/cdc/trigger.py` | Postgres trigger definitions |
| `benchling/cdc/models.py` | CDCEventLog, EventProcessor, EventSource models |
| `benchling/cdc/queue_records.py` | Generic CDC record processing |
| `benchling/cdc/queue_dedupe.py` | Redis-based deduplication |
| `benchling/warehouse/object_sync/queueing/tasks.py` | CDC → Job conversion |
| `benchling/warehouse/object_sync/queueing/jobs.py` | queue_object_sync_jobs |
| `benchling/warehouse/object_sync/queueing/create_object_sync_jobs.py` | Amplification logic |
| `benchling/warehouse/object_sync/queueing/batch_create_object_sync_jobs.py` | Batch amplification |
| `benchling/warehouse/object_sync/queueing/should_amplify.py` | Amplification rules |
| `benchling/warehouse/object_sync/sync_rows_to_warehouse.py` | Actual warehouse sync |
| `benchling/warehouse/object_sync/mapper_registry.py` | Table→Mapper registry with dependencies |

## Related Documentation

- [Warehouse Table Design Considerations](https://benchling.atlassian.net/wiki/spaces/BDI/pages/517406733) - explains denormalization
- [Warehouse Developer Guide](https://benchling.atlassian.net/wiki/spaces/BDI/pages/185173136)

