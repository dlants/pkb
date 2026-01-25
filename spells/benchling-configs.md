---
name: benchling-configs
description: Guide for Benchling configuration system, including deploy configs, tenant configs, and how to read/write config values
---

# Benchling Configuration System

Benchling uses a layered configuration system with multiple sources. The resolution order is defined in `ConfigSourceManager.build_ordered_initialized_sources()`.

## What is a Source?

A **source** is a dict-like object implementing `ConfigSource` (with `__contains__`, `__getitem__`, `keys()`). Each source wraps one way of providing config values (env vars, S3, database, Python defaults, etc.).

### Why Use get_config() Instead of Direct Access?

Nothing technically prevents `os.environ.get("FOO")` - the config system is a convention/best practice. Benefits of using `get_config()`:

1. **Consistent resolution** - Respects the priority order (higher-priority sources override lower ones)
2. **Observability** - `DefaultsSource` logs when defaults are used (helps debug "why is this value X?")
3. **Validation** - `set_config()` validates against config definitions
4. **Override stack** - Test code can use `override_configs()` to temporarily override values

### How is the Env Var Allowlist Enforced?

The `supported_env_vars()` allowlist in `sources.py` is only enforced by `create_env_source()`, which filters env vars before adding them to `env_source`. Direct `os.environ.get()` calls bypass this entirely.

The allowlist exists to:
1. Document which env vars are "official" and supported
2. Prevent accidental reliance on random env vars that might not be set in all environments
3. Keep the config system auditable (you can see what env vars affect the app)

To add a new supported env var, update `supported_env_vars()` in `benchling/configs/sources/sources.py` and add a default value in `config/deploy_config/base.py`.

## Resolution Order (Highest to Lowest Priority)

Defined in `ConfigSourceManager.build_ordered_initialized_sources()` in `/src/benchling/configs/sources/source_manager.py`.

When `get_config('CONFIG_NAME')` is called, sources are checked in this order (first match wins):

1. **override_stack_source** - Runtime overrides (for testing/debugging)
2. **env_source** - Environment variables (from `AURELIA_*` or bare env vars)
3. **s3_config_source** - S3 config bucket (`s3://{bucket}/{deploy}/config.json`)
4. **tenant_db_source** - Tenant-specific database overrides (`config_override` table with `tenant_id`)
5. **deploy_db_source** - Deploy-level database overrides (`config_override` table with `tenant_id = NULL`)
6. **test_source** - Test-only configs (when `TESTING=True`)
7. **dev_source** - Dev-only configs (when `DEBUG=True`)
8. **json_source** - Additional JSON config file
9. **infra_db_connections_source** - Computed DB connection strings
10. **infra_s3_bucket_names_source** - Computed S3 bucket names
11. **static_computed_source** - Computed configs from other config values
12. **runtime_computed_source** - Dynamically computed configs
13. **tenant_defaults_source** - Default values for tenant configs (from `TENANT_CONFIGS`)
14. **deploy_defaults_source** - Default values for deploy configs (from `DEPLOY_CONFIGS`)
15. **app_config_source** - Flask app config (from `Config`/`DefaultConfig` classes in `base.py`)

## Environment Variables Source

Environment variables are loaded via `create_env_source()` in `benchling/configs/sources/sources.py`.

Only allowlisted env vars are respected - see `supported_env_vars()` for the full list. Key ones include:
- `SENTRY_DSN_SERVER`, `SENTRY_DSN_JS`, `SENTRY_DSN_JS_NSB`
- `DEPLOY_NAME`, `DEPLOY_ENVIRONMENT`, `DEPLOY_ACCOUNT`, `DEPLOY_PARTITION`
- `REDIS_URI`, `CELERY_BROKER_URL`, `CELERY_RESULT_BACKEND`
- Database passwords: `DB_PASSWORD_APP`, `DB_PASSWORD_READONLY`, `DB_PASSWORD_SUPERUSER`

Env vars can be prefixed with `AURELIA_` or used bare (e.g., both `SENTRY_DSN_SERVER` and `AURELIA_SENTRY_DSN_SERVER` work).

### How Env Vars are Set (Infra Side)

Env vars come from `envvar_overrides` in the infra repo:
1. **Per-environment defaults**: `/infra/terraform/constants/deploy-features.json` - Sets defaults for all deploys in an environment
2. **Per-deploy overrides**: `/infra/terraform/constants/deploys.json` - Override for specific deploys

These are merged in `/infra/tf-modules/infra-metadata/s3-deploys.tf` (per-deploy values override per-environment).

Example from `deploys.json` showing Sentry disabled for dev-infrateam deploys:
```json
"dev-infrateam1b": {
  "envvar_overrides": {
    "SENTRY_DSN_SERVER": "",
    "SENTRY_DSN_JS": ""
  }
}
```

Empty string (`""`) means disabled/not configured.

## Config Types

### Deploy Configs (`OptionType.SYSTEM`)
- Apply to the entire deploy (all tenants)
- Stored in `config_override` table with `tenant_id = NULL`
- Defined in `benchling/configs/lib/deploy_configs.py`

### Tenant Configs (`OptionType.TENANT`)
- Apply to a specific tenant
- Stored in `config_override` table with a specific `tenant_id`

## Database Schema

```sql
                                          Table "public.config_override"
     Column     |            Type             | Collation | Nullable |                   Default
----------------+-----------------------------+-----------+----------+---------------------------------------------
 api_identifier | character varying(255)      |           | not null |
 id             | integer                     |           | not null | nextval('config_override_id_seq'::regspec)
 key            | character varying(1024)     |           |          |
 value          | jsonb                       |           | not null |
 tenant_id      | integer                     |           |          |
 modified_at    | timestamp without time zone |           | not null | now()
```

Key columns:
- `key` - Config name (e.g., 'SENTRY_DSN_SERVER', 'DISTRIBUTED_JOB_RETRY_LIMIT')
- `value` - JSON value
- `tenant_id` - NULL for deploy-level configs, tenant ID for tenant-specific configs

## Reading Configs

### Via run-script (Recommended - Gets Final Evaluated Value)

Use `scripts.configs.get_configs` via the run-scripts pipeline to get the final resolved value:

```bash
# Via Buildkite pipeline: https://buildkite.com/benchling/request-run-scripts-dev
COMMAND=scripts.configs.get_configs
TARGETS='<deploy-name>'
JSON_ARGS='["CONFIG_NAME_1", "CONFIG_NAME_2"]'
```

This resolves all sources (env vars, S3, database, defaults) and shows what the app actually sees.

### Via SQL (checking database overrides only)
```sql
SELECT key, value, tenant_id FROM config_override WHERE key = 'YOUR_CONFIG_NAME';
```

If no rows returned, the config uses a higher-priority source or falls back to defaults.

### Via infra config.py (S3 only!)

```bash
# From infra repo
./scripts/monolith/config.py --deploy <DEPLOY> read CONFIG_NAME
```

**Important**: This only shows S3 config values. It does NOT show:
- Environment variable overrides (from `deploys.json`)
- Database overrides (`config_override` table)
- Python defaults (`base.py`)

Code: `/infra/scripts/monolith/config.py`

## Writing Configs

### Via run-script (Database)
```bash
COMMAND=scripts.configs.set_deploy_config
TARGETS='<deploy-name>'
JSON_ARGS='{"config": "CONFIG_NAME", "value": <value>}'
```

### Via infra config.py (S3)
```bash
# From infra repo
./scripts/monolith/config.py --deploy <DEPLOY> write '{"CONFIG_NAME": <value>}'
```

Note: S3 configs require a web/celery restart to be picked up.

## Common Configs

### SENTRY_DSN_SERVER
- Where server-side Sentry errors are sent
- Source: Environment variable (from `deploys.json` envvar_overrides) OR `DefaultConfig` in `base.py`
- Default for dev-manual deploys (in `DefaultConfig`): `https://53a777cb6b302f8b391c3bef1e2edc3f@o8501.ingest.us.sentry.io/16148` (rate-limited)
- Default for production (in `DefaultConfig`): `https://b3006463b64f48b0847ab2247fa4b4a6@app.getsentry.com/16148`
- **Note**: Some deploys (e.g., dev-infrateam*) have `SENTRY_DSN_SERVER: ""` in `deploys.json`, which disables Sentry entirely (env var takes precedence over base.py defaults)

### DISTRIBUTED_JOB_RETRY_LIMIT
- Maximum retries for distributed jobs before marking as failed
- Default: `10`
- Component: `EngComponent.JOBS`

## Config Definition Example

From `benchling/configs/lib/deploy_configs.py`:
```python
ConfigInteger(
    "DISTRIBUTED_JOB_RETRY_LIMIT",
    visibility=Visibility.INTERNAL_ADMIN,
    editability=Editability.ENGINEERING,
    description="The maximum number of times a distributed job can be retried.",
    default_value=10,
    component=EngComponent.JOBS,
    option_type=OptionType.SYSTEM,
)
```

## Python Defaults (base.py)

The `Config` and `DefaultConfig` classes in `/src/config/deploy_config/base.py` provide the lowest-priority defaults.

`DefaultConfig` extends `Config` and adds environment-specific logic:
```python
class DefaultConfig(Config):
    # Use rate-limited sentry DSNs for dev-manual deploys
    if os.environ.get("AURELIA_DEPLOY_ENVIRONMENT") == "dev-manual":
        SENTRY_DSN_SERVER = "https://53a777cb6b302f8b391c3bef1e2edc3f@o8501.ingest.us.sentry.io/16148"
    else:
        SENTRY_DSN_SERVER = "https://b3006463b64f48b0847ab2247fa4b4a6@app.getsentry.com/16148"
```

These defaults are only used if no higher-priority source provides a value.

### S3 Bucket Location

The bucket name follows the pattern: `benchling-{account_prefix}-config-{region}`
- Dev account: `benchling-dev-config-us-west-2`
- Production: Uses account-specific prefix from `PER_REGION_BUCKET_PREFIXES_BY_ACCOUNT_ID`

Key path: `{DEPLOY_NAME}/config.json`

Example: `s3://benchling-dev-config-us-west-2/dev-infrateam1b/config.json`

### Inspecting S3 Config from an Adhoc

```bash
# List contents of deploy's config folder
aws s3 ls s3://benchling-dev-config-us-west-2/{DEPLOY_NAME}/

# Copy config to local disk for inspection
aws s3 cp s3://benchling-dev-config-us-west-2/{DEPLOY_NAME}/config.json /tmp/config.json

# Check for a specific config key
jq '.CONFIG_KEY // "NOT_FOUND"' /tmp/config.json

# Search for partial matches
grep -i search_term /tmp/config.json
```

### Local Cache

When the app starts, it caches the S3 config to `/opt/aurelia/cache/config.json`. However, on adhocs this file is typically empty since adhocs don't run the full app initialization.

### Code Reference

**Monolith (aurelia) repo:**
- Config source manager: `/src/benchling/configs/sources/source_manager.py` - Defines resolution order
- Config sources: `/src/benchling/configs/sources/sources.py` - Individual source implementations
- Initialization: `/src/benchling/configs/entrypoint/init_configs_pre_database.py` - How sources are created
- Python defaults: `/src/config/deploy_config/base.py` - `Config` and `DefaultConfig` classes
- Deploy config definitions: `/src/benchling/configs/lib/deploy_configs.py`
- Get configs script: `/src/scripts/configs/get_configs.py` - Run-script to read final resolved values
- S3 bucket names: `/src/benchling/lib/aws/s3_bucket_names.py`

**Infra repo:**
- Deploy definitions: `/infra/terraform/constants/deploys.json` - Per-deploy envvar_overrides
- Environment defaults: `/infra/terraform/constants/deploy-features.json` - Per-environment envvar_overrides
- S3 config script: `/infra/scripts/monolith/config.py` - Read/write S3 config source
- Terraform merge logic: `/infra/tf-modules/infra-metadata/s3-deploys.tf` - How envvar_overrides are merged

