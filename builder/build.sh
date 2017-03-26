#!/bin/sh

set -eu

gcloud auth activate-service-account \
	"$SERVICE_ACCOUNT" \
	--key-file="$SERVICE_ACCOUNT_KEY_FILE"

cd "$WORKSPACE/$BUILD_CONTEXT_DIR"
gcloud container builds submit --tag "gcr.io/${CLOUD_PROJECT}/$1:$2" .
