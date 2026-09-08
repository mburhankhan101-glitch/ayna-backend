# ayna-backend

Go modular monolith + AI worker (ADR-001, ADR-002). Deploys to Cloud Run,
Postgres on Neon.

**Steps 1–2 of the wiring order are complete.** The deploy pipeline is proven
before any domain code exists, and the API and event contracts are written and
enforced. What runs today is configuration, structured logging, a connection
pool, health probes, graceful shutdown, the module boundary lint, and the
contract drift tests. No business logic yet — deliberately.

## Layout

```
cmd/api/        composition root for the monolith
cmd/worker/     composition root for the AI worker
internal/
  modules/      one package per bounded context (see its README)
  platform/     cross-cutting, NOT business logic
    config/     env loading, validated at startup
    logger/     slog + correlation IDs (NFR-9)
    database/   pgx pool + readiness check
    health/     liveness vs readiness
    httpserver/ graceful shutdown
  arch/         module boundary tests — the rule that keeps ADR-001 honest
  contracts/    contract drift tests — openapi.yaml vs the event schemas
api/            openapi.yaml — the REST contract, and what the Flutter client generates from
events/         JSON Schema — the event envelope and the two riskiest payloads
deployments/    Dockerfile
```

## Running locally

```bash
cp .env.example .env      # then fill in DATABASE_URL from Neon
set -a; . ./.env; set +a
go run ./cmd/api
```

```bash
curl localhost:8080/healthz
curl localhost:8080/readyz
```

`/healthz` answers 200 whenever the process is running. `/readyz` answers 503
if Postgres is unreachable. That distinction is deliberate — see below.

Everything CI runs, run locally:

```bash
gofmt -l . && go vet ./... && go test -race ./...
```

## The contracts

`api/openapi.yaml` and `events/*.schema.json` are the agreement between the
Flutter client and this backend. They are not documentation written after the
fact — they are what both sides generate from, and `internal/contracts` fails
CI when they stop agreeing with each other.

Four decisions in them worth knowing before you write a handler:

**`Severity` has five values.** `unknown` means the analysis could not assess
that concern. It exists because the natural fallback is `none` — which tells
someone their skin is clear when nothing ever looked at it.

**`skinAge` and `heatmapUrl` are nullable.** The free tier genuinely has
neither (PD-3). Required fields would force the backend to invent a skin age,
and the obvious invention is the user's own age, which renders as "right in
step" and is false.

**`POST /scans` requires an `Idempotency-Key`.** Every accepted scan costs a
real vendor call, so a retry over a flaky connection would otherwise spend
twice and burn two of the user's weekly allowance.

**`GET /scans/{id}` returns `allowanceSpent`.** `false` for a rejected photo:
the vendor does not bill failed requests, and neither should you.

Generate the Dart client from the same file rather than hand-writing models —
that is the whole reason the spec exists.

## Five things here that are decisions, not boilerplate

**Liveness and readiness are different endpoints.** `/healthz` checks nothing
external; `/readyz` checks Postgres. If liveness pinged the database, a brief
database blip would make every instance look dead, the platform would restart
all of them at once, and a recoverable outage would become a total one.

**Config is validated at startup, and reports every problem at once.** A
missing `sslmode=require` is caught with a message naming the fix rather than
surfacing as an opaque error on the first query. One problem per restart is a
miserable way to configure a deploy.

**Graceful shutdown is a normal-path concern, not an edge case.** Cloud Run
scales to zero, so SIGTERM arrives on every scale-down, not just on deploys.
`SHUTDOWN_GRACE` is validated to stay under the platform's 10-second SIGKILL
window; exceeding it guarantees requests are cut off rather than drained.

**The worker has an HTTP server.** It runs on Cloud Run, which scales to zero,
and River is a *pull* queue — a process scaled to zero polls nothing. So the
API fires a fire-and-forget ping at `POST /wake` after committing, and Cloud
Scheduler hits the same endpoint as a safety net. The ping is an
**optimisation, never a correctness requirement**: the job is already committed
in Postgres in the same transaction as the scan, so a lost ping costs latency,
never a scan.

**The boundary lint is a test, not a convention.** `internal/arch` fails the
build if a `domain` package imports anything outside the standard library, or
if a module reaches into another module's `infrastructure`. Both rules are
verified against deliberate violations, because a checker that can never fail
looks exactly like a clean codebase.

## Deploying

CI (`.github/workflows/deploy.yml`) runs checks on every push and deploys both
services from `main`. It authenticates with **Workload Identity Federation**
rather than a service-account JSON key — a key in a repo secret never expires
and can't be revoked without first noticing it leaked.

Two Cloud Run settings that are cost controls rather than tuning:

- `ayna-worker` is `--no-allow-unauthenticated`. An open `/wake` endpoint would
  let anyone on the internet spin up instances and burn the free tier.
- `ayna-worker` is capped at `--max-instances=2`. Every instance calls a paid
  AI vendor, so unbounded scaling is unbounded spend (NFR-7).

### One-time setup

Neither Docker nor gcloud is installed on the machine this was scaffolded on,
so the steps below have **not been executed** — they are the sequence to run,
not a record of one.

```bash
# 1. Neon: create a project, copy the pooled connection string into .env
#    Region: AWS US West 2 (Oregon) — pairs with Cloud Run us-west1.

# 2. GCP
gcloud projects create ayna-prod --name="Ayna"
gcloud config set project ayna-prod
gcloud services enable run.googleapis.com artifactregistry.googleapis.com \
    secretmanager.googleapis.com

gcloud artifacts repositories create ayna \
    --repository-format=docker --location=us-west1

# 3. Store the database URL as a secret, never as an env var in the workflow
echo -n "postgres://...?sslmode=require" | \
  gcloud secrets create ayna-database-url --data-file=-

# 4. Workload Identity Federation for GitHub Actions, then set repo secrets:
#    GCP_PROJECT_ID, GCP_WIF_PROVIDER, GCP_SERVICE_ACCOUNT

# 5. Push to main.
```

Verify with the smoke test the workflow already runs:

```bash
curl -fsS "$(gcloud run services describe ayna-api \
  --region=us-west1 --format='value(status.url)')/readyz"
```

## Not done yet

No domain code. Steps 2–7 of the wiring order in
`09-Infrastructure-and-Services` add Auth0 + Resend, R2, River, Upstash, then
observability, then FCM. Each should leave something that runs.

The Dockerfile and the deploy workflow are **written but unexecuted** — no
Docker or gcloud on this machine. Expect the first real deploy to surface
something; that is what step 1 is for.
