I have a trace in /tmp/trace that is very large, hundreds of mb. It contains a trace for one of these schedule_jobs runs.

Let's try and poke around and see if we can filter out just the relevant spans.

Here's a relevant span, for example:

```
{"base_service":"src","benchling":{"component":"jobs","operation":"schedule_jobs","service":"aurelia"},"deploy":"mta-eu-central-1","deploy_env":"stable","duration":769430110,"env":"e-eu-central-1","git":{"commit":{"sha":"636efc41c6cd025118f8e78552b0e1aff4633866"},"repository":{"id":"github.com/benchling/aurelia"}},"language":"python","partition":"prod","service":"celery","service_context":"celery-a","service_override_type":"custom","stack":"e-eu-central-1","version":"636efc41c6cd025118f8e78552b0e1aff4633866"}
```

and an example of an irrelevant span:

```
{"base_service":"src","benchling":{"component":"warehouse","operation":"sync_to_warehouse_p201_p300","service":"aurelia"},"component":"redis","db":{"redis":{"database_index":0},"row_count":0,"system":"redis"},"deploy":"mta-eu-central-1","deploy_env":"stable","duration":1259956,"env":"e-eu-central-1","git":{"commit":{"sha":"636efc41c6cd025118f8e78552b0e1aff4633866"},"repository":{"id":"github.com/benchling/aurelia"}},"language":"python","network":{"destination":{"ip":"master.mta-eu-central-1-redis.aqjla5.euc1.cache.amazonaws.com","port":6379}},"partition":"prod","peer":{"db":{"system":"redis"},"hostname":"master.mta-eu-central-1-redis.aqjla5.euc1.cache.amazonaws.com","service":"monolith.task-worker"},"redis":{"args_length":2},"server":{"address":"master.mta-eu-central-1-redis.aqjla5.euc1.cache.amazonaws.com"},"service":"celery","service_context":"celery-b","service_override_type":"custom","span":{"kind":"client"},"stack":"e-eu-central-1","tenant":"lesaffre","tenant_subdomain":"lesaffre.benchling.com","version":"636efc41c6cd025118f8e78552b0e1aff4633866"}
```

## Trace structure investigation

File: `/tmp/trace` (~200MB, single JSON line with no line terminators)

Top-level keys:
- `trace` - contains `root_id` and `spans` (the main trace tree)
- `orphaned` - contains 22,631 spans that aren't connected to main trace
- `entities`, `span_id_to_entity_id`, `is_summary`, `is_truncated`, `summary_info`, `trace_truncation_reason`

Span structure (keyed by span_id):
- `trace_id`, `span_id`, `parent_id`
- `start`, `end`, `duration`
- `status`, `type`, `service`, `name`, `resource`
- `meta` - contains things like `benchling.operation`, `benchling.component`

The `benchling.operation` field in `meta` identifies what operation a span belongs to (e.g., `schedule_jobs`, `sync_to_warehouse_p201_p300`).

Most spans are orphaned (22,631) vs the main trace tree (only 1 span!).

### Detailed span structure (from first 5000 chars)

Each span has:
- `trace_id`, `span_id`, `parent_id` - linking info
- `start`, `end`, `duration` - timing
- `status`, `type`, `service`, `name`, `resource` - classification
- `meta` - flat key-value metadata with dotted keys like:
  - `benchling.operation` - e.g., "schedule_jobs", "queue_object_sync_jobs_p201_p300"
  - `benchling.component` - e.g., "monolith_infra", "warehouse"
  - `db.statement` - SQL/redis commands like `GET /obsync/px/1/schema_field_values_state/322513505`
  - `tenant`, `tenant_subdomain`
- `metrics` - numeric metrics

The single span in `.trace.spans` has `benchling.operation: "schedule_jobs"` but is actually a redis command span (type: redis, name: redis.command, resource: CLIENT). This is the root span that the trace was looked up by.

The 22,631 orphaned spans appear to be from child tasks (warehouse syncs etc.) that share the trace ID via distributed tracing but aren't structurally connected to the schedule_jobs root.

### Next steps

Need to filter orphaned spans to find those that:
1. Have parent_id chain leading back to schedule_jobs root, OR
2. Have `benchling.operation == "schedule_jobs"` in their meta

jq is too slow on this file. May need a streaming approach or Python script.
