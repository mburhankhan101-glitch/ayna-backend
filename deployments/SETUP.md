# First deploy to Cloud Run

One-time setup, then a single command to deploy. **Docker is not required** —
Cloud Build produces the image in the cloud.

Region is `us-west1` (Oregon), pairing with the Neon database in AWS
`us-west-2`, also Oregon. Both are inside Cloud Run's always-free tier.

---

## 1. Install gcloud

Roughly 500 MB. Nothing else needs installing.

```powershell
# Download and run the installer
Start-Process "https://dl.google.com/dl/cloudsdk/channels/rapid/GoogleCloudSDKInstaller.exe"
```

Or with winget, if you have it:

```powershell
winget install Google.CloudSDK
```

**Close and reopen PowerShell afterwards** so `gcloud` lands on PATH, then:

```powershell
gcloud version
gcloud auth login
```

---

## 2. Create the project

Project ids are globally unique across all of Google Cloud, so `ayna` is long
gone. Add something of your own — it never appears in the app.

```powershell
gcloud projects create ayna-prod-<something-unique> --name="Ayna"
gcloud config set project ayna-prod-<something-unique>
```

Then link billing. **This does not mean you will be charged** — Cloud Run's
free tier never expires, and the earlier modelling put your expected usage at
about 2% of it. But Google requires a billing account on file before Cloud Run
or Cloud Build will run at all.

```powershell
gcloud billing accounts list
gcloud billing projects link ayna-prod-<something-unique> --billing-account=<ACCOUNT_ID>
```

> Set a budget alert at $1 in the Cloud console. At this scale it should never
> fire, so if it does you want to hear about it the same day rather than at the
> end of the month.

---

## 3. Enable the APIs

```powershell
gcloud services enable `
    run.googleapis.com `
    cloudbuild.googleapis.com `
    artifactregistry.googleapis.com `
    secretmanager.googleapis.com
```

---

## 4. Create the image repository

```powershell
gcloud artifacts repositories create ayna `
    --repository-format=docker `
    --location=us-west1 `
    --description="Ayna service images"
```

---

## 5. Store the database URL as a secret

Never as an environment variable in a build file: env vars appear in build
logs, deploy history and `gcloud run services describe` output, and a Postgres
connection string carries a password.

```powershell
# Reads DATABASE_URL from your .env so the password is never typed at a prompt
# (where it would land in PowerShell history).
$url = (Get-Content .env | Where-Object { $_ -match '^DATABASE_URL=' }) -replace '^DATABASE_URL=',''
$url | gcloud secrets create ayna-database-url --data-file=-
```

Let the Cloud Run runtime service account read it:

```powershell
$proj = gcloud config get-value project
$num = gcloud projects describe $proj --format='value(projectNumber)'

gcloud secrets add-iam-policy-binding ayna-database-url `
    --member="serviceAccount:$num-compute@developer.gserviceaccount.com" `
    --role="roles/secretmanager.secretAccessor"
```

---

## 6. Let Cloud Build deploy to Cloud Run

Cloud Build can build by default but cannot deploy. Without these two grants
the build succeeds and the deploy step fails, which is a confusing way to
discover a permissions problem.

```powershell
$proj = gcloud config get-value project
$num = gcloud projects describe $proj --format='value(projectNumber)'
$cb = "$num-compute@developer.gserviceaccount.com"

gcloud projects add-iam-policy-binding $proj `
    --member="serviceAccount:$cb" --role="roles/run.admin"

gcloud projects add-iam-policy-binding $proj `
    --member="serviceAccount:$cb" --role="roles/iam.serviceAccountUser"
```

---

## 7. Deploy

```powershell
gcloud builds submit --config deployments/cloudbuild.yaml `
    --substitutions=_REGION=us-west1
```

First run takes a few minutes: it uploads the source, builds two images, and
deploys two services. Later runs are faster because the layers cache.

---

## 8. Check it

```powershell
$api = gcloud run services describe ayna-api --region=us-west1 --format='value(status.url)'
Write-Host $api

curl "$api/readyz"
```

`"postgres":"ok"` from `/readyz` means the deployed container reached Neon —
the same check that passed locally, now from Google's network.

**Do not curl `/healthz` on a `*.a.run.app` URL.** Google's front end answers that
exact path itself with its own HTML 404; the request never reaches the
container. Every neighbouring path (`/healthzz`, `/readyz`, `/v1/users`) does
reach it, which is how this was isolated. The route is still registered and
still works -- Cloud Run's own liveness probe talks to the container directly,
below the front end -- so this is a quirk of checking it from outside, not a
broken endpoint.

---

## 9. Point the app at it

```powershell
cd D:\ayna-app
flutter run --dart-define=API_BASE_URL=<the url from step 8>
```

No `adb reverse` needed any more, and no backend terminal. **The app now works
unplugged** — on your phone, or anyone else's, with your laptop switched off.

---

## Things that commonly go wrong

**`PERMISSION_DENIED` on the deploy step** — step 6 was skipped, or the grants
have not propagated yet. They take a minute.

**`Service Unavailable` on the first request** — Cloud Run cold start plus Neon
waking from scale-to-zero. The second request is fast. This is the same ~1s
you saw locally on the first `/readyz`.

**`/readyz` returns 503 with a postgres error** — the secret is missing or
unreadable. Check with:

```powershell
gcloud run services describe ayna-api --region=us-west1 --format='value(spec.template.spec.containers[0].env)'
```

**A 401 from the app** — the deployed service has its own `AUTH0_AUDIENCE`
from step 7. If it does not match the app's, every request fails with an error
that deliberately does not say why.

## Rotating the database credential

Neon invalidates the old password the moment you reset it, so production is
down between the reset and the Cloud Run update. Expect a few minutes.

```bash
# 1. Neon Console -> the branch whose endpoint is ep-odd-grass-af53tzce (main,
#    NOT dev) -> Connect -> Reset password. Both branches hold a database
#    called neondb, so check the ENDPOINT HOST, never the database name.

# 2. Store it. Note the sed: .env quotes the value, and a DSN that begins with
#    a double quote is one pgx cannot parse. It falls back to a unix socket as
#    user "nonroot", the container exits before binding the port, and Cloud Run
#    reports only "failed to start and listen on the port" -- which looks like
#    a port problem and is not. This exact mistake cost a deploy cycle.
grep -m1 '^DATABASE_URL=' .env \
  | sed -E 's/^DATABASE_URL=//; s/^"//; s/"$//' \
  | tr -d '\r\n' \
  | gcloud secrets versions add ayna-database-url --data-file=-

# 3. Check the stored value before deploying it. Cheaper than a failed rollout.
gcloud secrets versions access latest --secret=ayna-database-url | head -c 13   # postgresql://

# 4. New revisions. :latest is resolved at container start, so a running
#    revision keeps the old value until it is replaced.
gcloud run services update ayna-api    --region=us-west1 --update-secrets=DATABASE_URL=ayna-database-url:latest
gcloud run services update ayna-worker --region=us-west1 --update-secrets=DATABASE_URL=ayna-database-url:latest

# 5. Verify. /readyz is the one that touches Postgres.
curl -fsS "$(gcloud run services describe ayna-api --region=us-west1 --format='value(status.url)')/readyz"
```

If step 5 still reports `password authentication failed`, the reset landed on
the wrong branch and the exposed credential is still live. Go back to step 1.
