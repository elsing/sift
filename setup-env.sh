#!/usr/bin/env bash
# Fills in any missing secret in .env with a freshly generated random value.
# Safe to re-run — never touches a variable that's already set, so it won't
# rotate something already in use (e.g. POSTGRES_PASSWORD, which can't just be
# changed in .env once the db volume already exists — see the comment in .env).
#
# OIDC_* and POSTGRES_USER/POSTGRES_DB are not touched: they're not secrets with a
# sensible random default, they need real values from you.
set -euo pipefail
cd "$(dirname "$0")"

ENV_FILE=".env"
[ -f "$ENV_FILE" ] || cp .env.example "$ENV_FILE"

get_var() {
  grep -E "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2- || true
}

set_var() {
  local key="$1" val="$2"
  local escaped
  escaped=$(printf '%s' "$val" | sed -e 's/[\/&]/\\&/g')
  if grep -qE "^$key=" "$ENV_FILE"; then
    sed -i.bak "s/^$key=.*/$key=$escaped/" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
  else
    echo "$key=$val" >> "$ENV_FILE"
  fi
}

if [ -z "$(get_var POSTGRES_PASSWORD)" ]; then
  set_var POSTGRES_PASSWORD "$(openssl rand -base64 24 | tr -d '/+=')"
  echo "Generated POSTGRES_PASSWORD"
fi

if [ -z "$(get_var ENCRYPTION_KEY)" ]; then
  set_var ENCRYPTION_KEY "$(openssl rand -base64 32)"
  echo "Generated ENCRYPTION_KEY"
fi

if [ -z "$(get_var VAPID_PUBLIC_KEY)" ] || [ -z "$(get_var VAPID_PRIVATE_KEY)" ]; then
  read -r vapid_pub vapid_priv <<<"$(go run ./scripts/genvapid)"
  set_var VAPID_PUBLIC_KEY "$vapid_pub"
  set_var VAPID_PRIVATE_KEY "$vapid_priv"
  echo "Generated VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY"
fi

echo "Done. Still need real values for OIDC_ISSUER/OIDC_CLIENT_ID/OIDC_CLIENT_SECRET/OIDC_REDIRECT_URL in $ENV_FILE."
