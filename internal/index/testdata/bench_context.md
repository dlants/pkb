# Project Context

## Product

**gatherus** is an attendance tracking and post-event survey tool for after-school programs, nonprofits, libraries, and similar small organizations. Organizers run events, record who attended (including walk-ins), and send or display post-event surveys. See **[design/product.md](design/product.md)** for the short product description and **[plans/2026-04-18-wedge-pivot.md](plans/2026-04-18-wedge-pivot.md)** for the active implementation plan.

> The project previously targeted a full SIS (grading, authorization, org hierarchies, multi-tenant RLS). Those docs now live under `design/archive/` for reference only. The current codebase is mid-pivot — some legacy plumbing (WorkOS auth, Kysely, student routes) is still in place and gets removed phase-by-phase per the plan.

## Structure

TypeScript monorepo with three packages:

- **`packages/iso/`** — Shared types, branded IDs, API request/response contracts. Imported by both backend and frontend.
- **`packages/backend/`** — Fastify server. Entry point `app.ts` → `createApp` in `create-app.ts` (wires auth, email, domain plugins). Domain logic lives in `domains/` as Fastify plugins, each owning its own routes and DB queries.
- **`packages/frontend/`** — Vamp-based UI. Entry `main.ts` boots `startApp` in `views/app-shell.ts`. No tests live here; UI tests live in `packages/e2e/` and run against a real backend.
- **`packages/test/`** — Shared test primitives (`@gatherus/test`): pool-backed builder, composites, DB lifecycle (`withTestDb` per-test + `withSharedTestDb` one-per-process with lazy init and process-exit teardown), per-phase timing counters (`getTestDbTimings`), captured-email storage, port/tag-file helpers, cleanup CLI. Consumed by both backend vitest and `@gatherus/e2e`.
- **`packages/e2e/`** — Playwright end-to-end suite (`@gatherus/e2e`). One DB per `npm run test:e2e` invocation; per-test isolation via unique orgs/emails. See **[.magenta/skills/frontend-testing/skill.md](.magenta/skills/frontend-testing/skill.md)** for the full strategy and the `FixtureBuilder` / interactions contract. Backend-side testing is documented separately in **[.magenta/skills/backend-testing/skill.md](.magenta/skills/backend-testing/skill.md)**.

## Key Constraints

- **Pre-launch / MVP — no backwards compatibility burden.** The project is in early development with no customers and no production data to persist. There is no migration, data-retention, or API-compatibility obligation: prefer the cleanest end-state design over incremental, compatibility-preserving change. Feel free to blow away and rebuild schemas, endpoints, iso contracts, and stored data when it yields a simpler design (drop-and-recreate migrations are fine; no need for additive-only columns or dual-read/dual-write transition phases).

- **App-level `org_id` filtering.** Every org-scoped table has `org_id` and every query filters by it. No RLS at MVP; revisit if/when we take on customer-managed encryption or very sensitive data.

- **Complex writes, simple reads.** When state lives in an object graph (groups → events → survey*instances → responses, etc.), propagate changes \_at write time* so every read path can stay a flat single-table filter. The canonical case is soft delete: deleting a parent must soft-delete every descendant in one transactional write, so every read can just say `WHERE deleted_at IS NULL` on the table it cares about and never has to walk back up the chain to check ancestor liveness. Example: `softDeleteEventCascade` writes `deleted_at` to the event, its `survey_instances`, and their `responses` in a single CTE; the planned `softDeleteEventGroupCascade` extends the same chain up to the group. Prefer this shape over read-side joins (`AND NOT EXISTS (... deleted parent ...)`) — reads are the hot path and run far more often than writes.

- **Backend types ≠ protocol types — never collapse them.** Two parallel type worlds, owned by different files, that must stay disjoint:
  - **DB types** are _generated_ by pgtyped from `.sql` query files against the real Postgres schema. They live next to their queries (e.g. `packages/backend/<domain>/queries/*.types.ts`) and reflect column nullability, jsonb shape, etc. exactly. They are never imported into `packages/iso/` and never sent over the wire.
  - **Iso (protocol) types** are _hand-written_ in `packages/iso/types.ts` and describe the JSON contract between backend and frontend (request bodies, response payloads, branded ids, discriminated unions like `FieldDef`). They are never imported into pgtyped query definitions and never used as the row type for a SQL result.
  - **Mapping is explicit and lives in the route handler / domain plugin.** Every handler that returns data does an explicit `dbRow → IsoType` mapping, and every handler that accepts a request does an explicit `IsoType → query params` mapping. No `as` casts between the two worlds, no shared helper that "is both", no re-exporting a pgtyped row as an iso type.
  - **Why:** the DB shape changes for storage reasons (denormalization, jsonb migrations, NOT NULL backfills), and the protocol changes for product reasons (versioning, additive fields). Coupling them means every storage tweak ships as a breaking API change, and every frontend rename pressures the schema. Keeping them disjoint is the load-bearing rule that lets us evolve each independently.

- **`Result<T>` for fallible operations.** in `packages/iso/types.ts`; use it for validators/parsers/builders instead of `T | null` or throwing.

- **Branded ids over bare `string`.** `FieldId = string & { readonly __brand: "FieldId" }` When used as a key, document it like so `Record<string /* FieldId */, unknown>`.

- **Single global dispatch.** Every message in the frontend routes through the root `dispatch` loop in `main.ts` — pages never spin up their own internal `dispatch = (msg) => { update(...); sync(...) }` closure. Page-local messages get wrapped at the page boundary as `{ type: "PAGE_MSG", run: () => update(this.state, msg, dispatch, ...) }` and forwarded to the parent dispatch;

### Shared / platform

- **Auth**: `packages/backend/auth/setup.ts` (better-auth). Frontend login at `views/login.ts`.
- **Email**: `packages/backend/email/send.ts` (Resend, injectable `EmailSender`).
- **DB migrations**: `packages/backend/db/migrations/*.sql` (dbmate). Most recent: `20260419180000_survey_versions.sql`.
- **Frontend primitives**: `packages/frontend/vamp.ts` (Binder + `bindX` helpers, `show`/`showKeyed`, `sanitize`, `ref`, `cls`). Router in `packages/frontend/router.ts`. Test-mode toggles in `packages/frontend/env.ts` (`timers.saveDebounceMs` / `timers.searchDebounceMs` collapse to 0 when `VITE_TEST_MODE=1`, which Playwright's `webServer` sets).

### Organizations / memberships

- **Iso**: `Organization`, `MembershipRole`, `MeResponse`, `CreateOrganizationRequest`.
- **Backend**: `packages/backend/organizations/`, `packages/backend/memberships/` (with `check.ts` for `requireMembership`). Org-create transactionally inserts a `'Default attendance template'` (empty schema, v1 published) and stamps `organizations.default_attendance_template_id`; existing orgs were backfilled by `20260524180000_attendance.sql`. New events fall back to this template when their group has no `attendance_survey_template_id` override.
- **Frontend**: org picker + create-org form live in `views/app-shell.ts`.

### Organizations / invitations

- **Iso**: `OrgInvitation`, `CreateOrgInvitationRequest`, `ListMembersResponse`, `ListOrgInvitationsResponse`, `AcceptOrgInvitationResponse`, `PublicOrgInvitation`.
- **Backend**: `packages/backend/org-invitations/` domain plugin (`routes.ts` + `queries/*.sql`), endpoints under `/api/organizations/:orgId/{members,invitations}`, `/api/invitations/:token`, `/api/invitations/:token/accept`. Re-uses `EmailSender`. Owner-only mutations via `requireOwner` in `packages/backend/memberships/check.ts`. Migration: `20260420000000_org_invitations.sql`. Tests: `packages/backend/org-invitations/routes.test.ts`.
- **Frontend**: `views/organization.ts` (owner sees members/pending invites/invite form; member sees just the org header), `views/accept-invite.ts` (landing page for `/invite/:token`; embeds `views/auth-form.ts` with locked email when logged out). Routes `/org` and `/invite/:token` in `router.ts`. `api.ts`: `listMembers`, `listOrgInvitations`, `createOrgInvitation`, `deleteOrgInvitation`, `getOrgInvitationByToken`, `acceptOrgInvitation`. E2e tests: `packages/e2e/tests/organization.spec.ts`, `packages/e2e/tests/accept-invite.spec.ts`, `packages/e2e/tests/org-invite-flow.spec.ts`.

### Attendees

- **Iso**: `Attendee`, `AttendeeSchema`, `CreateAttendeeRequest`, `UpdateAttendeeRequest`, `MergeAttendeesRequest`, `ListAttendeesResponse`.
- **Backend**: `packages/backend/attendees/` (routes, queries, `validate-custom-fields.ts` — the canonical `switch (field.type)` used by response submit).
- **Frontend**: the `/attendees` list view is the scoped `SearchPage` (`pages/search.ts`, `scope: "attendees"` → `facetFilters: { type: ["attendee"] }`); attendee cards link to `/attendees/:id`. The legacy `pages/attendees-list.ts` (table list, CSV import, multi-select/merge) is unmounted and slated for deletion once its bulk-action behaviors are ported into the search view's selection model (see `plans/2026-06-09-attendees-search-migration.md`). `views/attendee-detail.ts` is the detail page.

### Events

- **Iso**: `Event`, `EventWithCounts`, `EventDetail`, `CreateEventRequest`, `UpdateEventRequest`, `RepairAttendanceRequest`, `ListEventsResponse`.
- **Backend**: `packages/backend/events/`. Every event has an `attendance_instance_id` pointing at its designated attendance survey instance, auto-created in the event-create transaction (template = `event_group.attendance_survey_template_id ?? organizations.default_attendance_template_id`, version-pinned at that moment).
- **Frontend**: `views/events-list.ts`, `pages/event-detail.ts` (elevates the attendance instance into its own header with counts + "Take attendance" CTA; "Additional surveys" filters out the attendance instance). E2e tests: `packages/e2e/tests/events-list.spec.ts`, `packages/e2e/tests/event-detail.spec.ts`.

### Surveys (schema + draft/publish lifecycle)

- **Iso**: `Survey`, `SurveyVersion`, `SurveyWithCounts`, `Schema`, `FieldDef` (discriminated union — add cases here and every exhaustive `switch` in the code will complain), `UpdateSurveyRequest`, `UpdateDraftRequest`, `PublishDraftResponse`, `assertNever`.
- **Backend**: `packages/backend/surveys/routes.ts` (CRUD + `PUT /api/surveys/:id/draft`, `POST /api/surveys/:id/publish`, `DELETE /api/surveys/:id/draft`, CSV export, invitation send), `surveys/queries/*.sql` (`create-survey`, `create-survey-version`, `get-survey-with-version`, `update-draft`, `clear-draft`, `set-current-version`, `get-max-version-number`, `list-surveys-with-version`, `count-responses`, …), `surveys/routes.test.ts` (draft/publish lifecycle tests), `surveys/public.ts` (public GET/submit endpoints — always reads `current_version.schema`, tags responses with `survey_version_id`).
- **Frontend — builder**: `views/survey-builder.ts` (state machine, auto-save debounce, version badge, publish/discard, `FieldRowView` + per-type editor sub-views, `ChoiceFieldEditor`, `OptionRowView`). E2e tests: `packages/e2e/tests/survey-builder.spec.ts`.
- **Frontend — detail**: `views/survey-detail.ts` (responses table, reconcile queue, CSV link, public URL + QR, version badge, "Edit questions" link). E2e tests: `packages/e2e/tests/survey-detail.spec.ts`.
- **Frontend — public**: `views/public-survey.ts` (per-`FieldDef` input rendering incl. the `name` composite `{given_name, family_name}` branch). E2e tests: `packages/e2e/tests/public-survey.spec.ts`.
- **Frontend — api**: `packages/frontend/api.ts` (`getSurvey`, `createSurvey`, `updateSurvey`, `updateDraft`, `publishDraft`, `discardDraft`, `submitResponse`, `listSurveyResponses`, `reconcileSurvey`, `matchResponse`, `promoteResponse`, `sendInvitations`, `surveyCsvUrl`).

### Responses

- **Iso**: `Response`, `ResponseWithAttendee`, `CreateResponseRequest` (attendee_id | new_attendee | anonymous), `MatchResponseRequest`, `PromoteResponseRequest`, `ReconcileEntry`, `ReconcileResponse`.
- **Backend**: `packages/backend/responses/` (queries: `create-response`, `create-response-with-walk-in`, `check-existing-response`, `list-by-survey`, `match-response`, `get-response`). Response routes are defined alongside surveys in `surveys/routes.ts` (`POST /api/surveys/:id/responses`, `GET /api/surveys/:id/responses`, `GET /api/surveys/:id/responses.csv`, `POST /api/responses/:id/match`).
- **Frontend**: rendered in `views/survey-detail.ts` (list + reconcile) and `views/event-detail.ts` (check-in feed).

### Invitations

- **Iso**: `Invitation`, `SendInvitationsRequest`, `SendInvitationsResponse`, `PublicSurveyWithInvitation`.
- **Backend**: `packages/backend/invitations/queries/`, send flow in `surveys/routes.ts` (`POST /api/surveys/:id/send`). Magic-link consumption in `surveys/public.ts`.
- **Frontend**: "Send invitations" button in `views/survey-detail.ts`; invited respondents hit `views/public-survey.ts` via `/s/:slug/r/:token`.

### Routing

All frontend routes live in `packages/frontend/router.ts`. The nav scopes `/attendees`, `/surveys`, `/events`, `/rosters` all parse to `{ page: "search", scope }` and render the scoped `SearchPage`; `/search` is the unscoped box. Other pages: `login`, `home`, `attendee` (detail), `event`, `survey-edit`, `public-survey`. `app-shell.ts` dispatches each route to a view class via `mountView`.

### Database migrations

New `.sql` files in `packages/backend/db/migrations/` must be applied to the running local Postgres before the backend can use them — `pgtyped` generates the types statically from your SQL, but the actual schema still has to change on disk. Run:

```
cd packages/backend
set -a && . /Users/denis.lantsman/src/gatherus/.env && set +a
npm run migrate
```

(The repo-root `.env` defines `DATABASE_URL`; dbmate reads it via the script's `--url $DATABASE_URL`.) After this, restart the backend so its prepared statements pick up the new columns. The e2e suite runs its own `dbmate up` against an isolated `gatherus_test_*` DB at the start of each run, so e2e tests do not need this step.

To create a new migration: `npm run migrate:new <name>` from `packages/backend`. Rollback the latest with `dbmate --url $DATABASE_URL down`.

### Typechecking

We use `tsgo`. Always use `npm run typecheck` — never `npx tsc --noEmit`.
