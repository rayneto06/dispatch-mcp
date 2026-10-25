#!/usr/bin/env bash
# Deploys dispatch-mcp and fakefleet to Cloud Run in their own project.
# Safe to re-run: every step checks before it creates.
#
#   PROJECT_ID=my-project BILLING_ACCOUNT=XXXXXX-XXXXXX-XXXXXX bash deploy/deploy.sh
#
# BILLING_ACCOUNT is only needed when the project does not exist yet.
# Set YES=1 to skip the confirmation prompt (needed when there is no terminal,
# e.g. when run with "!" from Claude Code).
set -euo pipefail

: "${PROJECT_ID:?set PROJECT_ID}"
REGION=${REGION:-us-central1}
REPO=dispatch
SA_NAME=dispatch-run
SECRETS=(partner-api-key webhook-secret mcp-token)

gc() { gcloud --project "$PROJECT_ID" --quiet "$@"; }

account=$(gcloud config get-value account 2>/dev/null)
parent=$(gcloud projects describe "$PROJECT_ID" --format='value(parent.type,parent.id)' 2>/dev/null || echo "(new project)")
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

# 4. Secrets: random values, created once, never printed except the MCP URL at the end.
for s in "${SECRETS[@]}"; do
  if ! gc secrets describe "$s" >/dev/null 2>&1; then
    openssl rand -hex 32 | tr -d '\n' | gc secrets create "$s" --data-file=- --replication-policy=automatic
  fi
done

# 5. A runtime service account that can read those three secrets and nothing else.
SA="$SA_NAME@$PROJECT_ID.iam.gserviceaccount.com"
if ! gc iam service-accounts describe "$SA" >/dev/null 2>&1; then
  gc iam service-accounts create "$SA_NAME" --display-name="dispatch-mcp runtime"
fi
for s in "${SECRETS[@]}"; do
  gc secrets add-iam-policy-binding "$s" --member="serviceAccount:$SA" \
    --role=roles/secretmanager.secretAccessor >/dev/null
done

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

common=(--region="$REGION" --image="$IMAGE" --service-account="$SA"
  --allow-unauthenticated --min-instances=0 --max-instances=1 --cpu-boost)

# fakefleet advances statuses and sends webhooks from background goroutines,
# so it needs CPU outside requests (instance-based billing).
gc run deploy fakefleet "${common[@]}" --command=/fakefleet --no-cpu-throttling \
  --set-secrets=PARTNER_API_KEY=partner-api-key:latest,WEBHOOK_SECRET=webhook-secret:latest \
  --set-env-vars=WEBHOOK_URL="$DISPATCH_URL"

gc run deploy dispatch-mcp "${common[@]}" \
  --set-secrets=PARTNER_API_KEY=partner-api-key:latest,WEBHOOK_SECRET=webhook-secret:latest,MCP_TOKEN=mcp-token:latest \
  --set-env-vars=TRANSPORT=http,FLEET_BASE_URL="$FLEET_URL"

TOKEN=$(gc secrets versions access latest --secret=mcp-token)
echo
echo "fakefleet health:    $FLEET_URL/healthz"
echo "dispatch-mcp health: $DISPATCH_URL/healthz"
echo "MCP URL (keep private): $DISPATCH_URL/mcp/$TOKEN"
