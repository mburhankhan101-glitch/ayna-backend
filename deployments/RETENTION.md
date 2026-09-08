# Photo retention sweep (NFR-4)

Onboarding tells every user their photos are "deleted on a schedule you
choose". This is the schedule. Without the Cloud Scheduler job below, the
column and the setting exist but nothing ever runs, and the sentence goes back
to being false.

## What runs

`POST /internal/retention/sweep` on the **worker** service. It reads each
user's retention policy, groups users by it, and clears stored overlay images
older than the policy window. One `UPDATE` per policy value, so three
statements regardless of how many accounts exist.

It clears the image and keeps the row. Scores and the trend survive: a user
asking for their photos to be deleted is not asking for their history to be
erased.

## Why there is no shared secret

The worker is deployed `--no-allow-unauthenticated`. Cloud Run rejects any
caller without a valid OIDC token from a service account holding
`roles/run.invoker`, before the request reaches Go. A hand-rolled secret header
on top would be a weaker mechanism in front of a stronger one, plus a secret to
rotate.

That is also why the scheduler job below needs its own service account and an
`--oidc-service-account-email` flag. Without those it gets a 403, which is the
system working.

## One-time setup

Run these once. `PROJECT_ID` and `REGION` should match the rest of the deploy.

```bash
gcloud services enable cloudscheduler.googleapis.com
```

Create the identity the scheduler calls as:

```bash
gcloud iam service-accounts create ayna-scheduler --display-name="Ayna Cloud Scheduler"
```

Let it invoke the worker, and nothing else:

```bash
gcloud run services add-iam-policy-binding ayna-worker --region=us-west1 --member="serviceAccount:ayna-scheduler@$(gcloud config get-value project).iam.gserviceaccount.com" --role=roles/run.invoker
```

Create the daily job. 03:00 Karachi is deliberate: it is off-peak for the only
users this product has, and a sweep that runs while someone is reading a report
could clear the overlay out from under them.

```bash
gcloud scheduler jobs create http ayna-retention-sweep --location=us-west1 --schedule="0 3 * * *" --time-zone="Asia/Karachi" --uri="$(gcloud run services describe ayna-worker --region=us-west1 --format='value(status.url)')/internal/retention/sweep" --http-method=POST --oidc-service-account-email="ayna-scheduler@$(gcloud config get-value project).iam.gserviceaccount.com" --attempt-deadline=120s
```

## Check it

Force a run without waiting for 3am:

```bash
gcloud scheduler jobs run ayna-retention-sweep --location=us-west1
```

Then read what it did. The response body carries per-policy counts, and the
worker logs one line per cohort:

```bash
gcloud logging read 'resource.labels.service_name="ayna-worker" AND jsonPayload.msg=~"retention"' --limit=20 --freshness=1h
```

A healthy first run on a fresh database reports `cleared: 0`. That is not a
failure: it means no stored image has yet outlived its window.

## What a failure looks like

The handler answers **500** on any cohort failure, so the job shows as failed
in the scheduler and retries. The body still carries the counts for the
cohorts that did succeed, because a partial sweep and a sweep that did nothing
are different problems and the log should not blur them.

The one failure mode to actually worry about is silence: the job succeeding
every night with `cleared: 0` forever, while images pile up. That means the
cohorts query is returning nothing — check that `photo_retention_days` is
populated, since a NULL there means "keep indefinitely" and is skipped by
design.

## Migration

The column ships in `00006_photo_retention.sql`, which also backfills existing
users to 30 days rather than NULL. Backfilling NULL would have handed every
current account the weakest policy in the product without them choosing it.

```bash
go run ./cmd/migrate up
```
