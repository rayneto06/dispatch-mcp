#!/usr/bin/env bash
# Deploys dispatch-mcp and fakefleet to Cloud Run in their own project.
# Re-running is safe once a deploy has succeeded, or after a failed build or
# service deploy: every step checks before it creates. A first run that
# fails while linking billing or creating the secrets can leave the project
# half set up; finish that step by hand, or delete the project and start over.
#
#   PROJECT_ID=my-project BILLING_ACCOUNT=XXXXXX-XXXXXX-XXXXXX bash deploy/deploy.sh
#
# BILLING_ACCOUNT is only needed when the project does not exist yet.
# Set YES=1 to skip the confirmation prompt (needed when there is no terminal,
# e.g. when run with "!" from Claude Code).
set -euo pipefail

# The build uploads the current directory, so work from the repo root
# wherever the script is started from.
cd "$(dirname "$0")/.."

: "${PROJECT_ID:?set PROJECT_ID}"
REGION=${REGION:-us-central1}
REPO=dispatch
SECRETS=(partner-api-key webhook-secret mcp-token)

gc() { gcloud --project "$PROJECT_ID" --quiet "$@"; }

ENV_FILE="$PWD/.env"

# save_env KEY VALUE FILE sets KEY=VALUE in FILE, keeping its other lines,
# and leaves the file readable only by its owner.
save_env() {
  local key=$1 value=$2 file=$3
  (
    umask 077
    { grep -v "^$key=" "$file" 2>/dev/null || true; echo "$key=$value"; } > "$file.tmp"
    mv "$file.tmp" "$file"
  )
}

# IAM is eventually consistent: a service account created a moment ago can be
# "not found" by the next call. Retry with a growing pause.
retry() {
  local i
  for i in 1 2 3 4 5 6; do
    "$@" && return 0
    echo "retrying in $((i * 5))s..." >&2
    sleep $((i * 5))
  done
  return 1
}

account=$(gcloud config get-value account 2>/dev/null)
parent=$(gcloud projects describe "$PROJECT_ID" --format='value(parent.id)' 2>/dev/null || echo "(new project)")
echo "account: $account"
echo "project: $PROJECT_ID  parent: ${parent:-none}  region: $REGION"
if [[ "${YES:-}" != 1 ]]; then
  read -r -p "Deploy to $PROJECT_ID as $account? [y/N] " answer
  [[ "$answer" == y ]] || { echo "aborted"; exit 1; }
fi

# 1. Project and billing.
if ! gcloud projects describe "$PROJECT_ID" >/dev/null 2>&1; then
  : "${BILLING_ACCOUNT:?project does not exist: set BILLING_ACCOUNT to create it}"
  gcloud projects create "$PROJECT_ID" --name=dispatch-mcp
  gcloud billing projects link "$PROJECT_ID" --billing-account="$BILLING_ACCOUNT"
fi
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')

# 2. APIs.
gc services enable run.googleapis.com cloudbuild.googleapis.com \
  artifactregistry.googleapis.com secretmanager.googleapis.com

# 3. Image repository.
if ! gc artifacts repositories describe "$REPO" --location="$REGION" >/dev/null 2>&1; then
  gc artifacts repositories create "$REPO" --repository-format=docker --location="$REGION"
fi

# 4. Secrets: random values, created once, never printed. The MCP URL, which
# carries the token, is written to .env at the end.
for s in "${SECRETS[@]}"; do
  if ! gc secrets describe "$s" >/dev/null 2>&1; then
    openssl rand -hex 32 | tr -d '\n' | gc secrets create "$s" --data-file=- --replication-policy=automatic
  fi
done

# 5. One runtime service account per service, each able to read only the
# secrets it uses: the public fakefleet never holds the MCP token.
DISPATCH_SA="dispatch-run@$PROJECT_ID.iam.gserviceaccount.com"
FLEET_SA="fakefleet-run@$PROJECT_ID.iam.gserviceaccount.com"
ensure_sa() { # ensure_sa NAME DISPLAY_NAME
  if ! gc iam service-accounts describe "$1@$PROJECT_ID.iam.gserviceaccount.com" >/dev/null 2>&1; then
    gc iam service-accounts create "$1" --display-name="$2"
  fi
}
grant() { # grant SECRET SERVICE_ACCOUNT
  retry gc secrets add-iam-policy-binding "$1" --member="serviceAccount:$2" \
    --role=roles/secretmanager.secretAccessor >/dev/null
}
ensure_sa dispatch-run "dispatch-mcp runtime"
ensure_sa fakefleet-run "fakefleet runtime"
for s in "${SECRETS[@]}"; do grant "$s" "$DISPATCH_SA"; done
for s in partner-api-key webhook-secret; do grant "$s" "$FLEET_SA"; done

# 6. Build the image in Cloud Build (no local Docker needed). New projects build
# as the Compute default service account; it needs these roles to push and log.
BUILD_SA="$PROJECT_NUMBER-compute@developer.gserviceaccount.com"
for role in roles/artifactregistry.writer roles/logging.logWriter roles/storage.objectViewer; do
  gc projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:$BUILD_SA" \
    --role="$role" --condition=None >/dev/null 2>&1 \
    || echo "note: could not grant $role to $BUILD_SA (fine if the build below works)"
done
TAG=$(git rev-parse --short HEAD 2>/dev/null || date +%Y%m%d%H%M%S)
IMAGE="$REGION-docker.pkg.dev/$PROJECT_ID/$REPO/app:$TAG"
gc builds submit --tag "$IMAGE" .

# 7. Cloud Run URLs are deterministic, so each service can be told the other's
# URL before either exists.
DISPATCH_URL="https://dispatch-mcp-$PROJECT_NUMBER.$REGION.run.app"
FLEET_URL="https://fakefleet-$PROJECT_NUMBER.$REGION.run.app"

common=(--region="$REGION" --image="$IMAGE"
  --allow-unauthenticated --min-instances=0 --max-instances=1 --cpu-boost)

# fakefleet advances statuses and sends webhooks from background goroutines,
# so it needs CPU outside requests (instance-based billing).
gc run deploy fakefleet "${common[@]}" --service-account="$FLEET_SA" --command=/fakefleet --no-cpu-throttling \
  --set-secrets=PARTNER_API_KEY=partner-api-key:latest,WEBHOOK_SECRET=webhook-secret:latest \
  --set-env-vars=WEBHOOK_URL="$DISPATCH_URL"

gc run deploy dispatch-mcp "${common[@]}" --service-account="$DISPATCH_SA" \
  --set-secrets=PARTNER_API_KEY=partner-api-key:latest,WEBHOOK_SECRET=webhook-secret:latest,MCP_TOKEN=mcp-token:latest \
  --set-env-vars=TRANSPORT=http,FLEET_BASE_URL="$FLEET_URL"

# The MCP URL carries the token, so it goes to the gitignored .env instead of
# the terminal, where it would end up in scrollback and logs.
TOKEN=$(gc secrets versions access latest --secret=mcp-token)
save_env MCP_URL "$DISPATCH_URL/mcp/$TOKEN" "$ENV_FILE"
echo
echo "fakefleet health:    $FLEET_URL/health"
echo "dispatch-mcp health: $DISPATCH_URL/health"
echo "MCP URL:             $DISPATCH_URL/mcp/${TOKEN:0:4}… (full URL saved to $ENV_FILE as MCP_URL)"
