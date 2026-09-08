# ayna-backend

Go modular monolith + worker (ADR-001, ADR-002) for **Ayna** (آئینہ, "mirror" in
Urdu) — an AI skin analysis app. Cloud Run, Postgres on Neon, Auth0 for
identity.

> Part of a four-repo project:
> **[ayna-backend](https://github.com/mburhankhan101-glitch/ayna-backend)** (you are here) ·
> [ayna-app](https://github.com/mburhankhan101-glitch/ayna-app) ·
> [ayna-spike](https://github.com/mburhankhan101-glitch/ayna-spike) ·
> [ayna-docs](https://github.com/mburhankhan101-glitch/ayna-docs)
>
> Start with [ayna-docs](https://github.com/mburhankhan101-glitch/ayna-docs) for
> why any of this is shaped the way it is.

Portfolio project, not a business.

**The whole journey runs on live infrastructure.** Sign in → capture → analyse
→ report → history → trend, against a real paid vision vendor, deployed and
serving. Two modules (`iam`, `skinanalysis`), ~100 tests, plus two test suites
that fail the build on architectural drift rather than on behaviour.

## Layout

```
cmd/api/        composition root for the monolith
cmd/worker/     composition root for the AI worker
internal/
  modules/
    iam/        accounts, the 18+ gate, consent, allowance, photo retention
    skinanalysis/ scan submission, the vendor adapter, overlays, trend
                  each: domain/ application/ infrastructure/
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
migrations/     goose migrations
deployments/    Dockerfile, cloudbuild.yaml, SETUP.md, RETENTION.md
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

## Seven things here that are decisions, not boilerplate

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

**Modules never import each other.** `skinanalysis` declares the `Identity`
port it needs, `iam` declares `AllowanceSource`, and `cmd/api/{identity,
allowance}.go` — the composition root, the one place allowed to know both —
satisfies each. The retention sweep in `cmd/worker/retention.go` is the same
pattern: it needs each user's policy (iam) and which stored images are past it
(skinanalysis), and a `users ⋈ scans` join would have passed the boundary lint,
because that lint reads Go imports and not SQL tables, while welding the two
modules together exactly as the rule forbids.

**The worker is closed to the internet and authenticated by the platform.** It
runs `--no-allow-unauthenticated`, so Cloud Run rejects any caller without a
valid OIDC token from a service account holding `roles/run.invoker` before the
request reaches Go. There is no shared secret in the handler on purpose: a
hand-rolled header check would be a weaker mechanism sitting in front of a
stronger one, plus a secret to rotate.

**API concurrency is 8, not the default 80.** `photo.TrimTall` decodes JPEGs,
so a scan holds 15–20MB in flight; 80 of those against a 512Mi instance is an
out-of-memory kill, not throughput.

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

This has been executed; the service is live. `deployments/SETUP.md` is the
worked version with the mistakes that actually came up, and
`deployments/RETENTION.md` covers the scheduled photo-retention sweep.

The deploy job is gated on a `DEPLOY_TO_CLOUD_RUN` repository variable, so a
fresh clone runs the checks and stops there rather than failing on GCP secrets
it has no reason to hold.

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

- **Store billing.** No Billing module. The app's paywall says "Plus is not on
  sale yet" and means it. `entitlement.tier` is still hardcoded to `free`;
  the *counts* are not, and that distinction is the point — see above.
- **Email OTP** (PD-3). Sign-in currently uses Auth0's Google connection.
- **The retention sweep has no scheduler yet.** The endpoint, the policy and
  the setting all exist; until the Cloud Scheduler job in
  `deployments/RETENTION.md` is created, nothing calls it.
- **R-1 is open.** The vendor's acne score may under-report. Until one
  controlled photograph settles it, the report screen must not lead on
  breakouts. See
  [ADR-003](https://github.com/mburhankhan101-glitch/ayna-docs/blob/main/06-AI-Provider-Evaluation.md).

Two files describe one deployment — `deployments/cloudbuild.yaml` and
`.github/workflows/deploy.yml`. They have drifted once already (the workflow
sat at `--concurrency=80` with no vendor API key long after the scan path began
decoding images), so keep them in step or collapse them.
