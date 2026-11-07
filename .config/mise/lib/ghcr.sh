# shellcheck shell=bash
# ghcr_manifest_status <image> <tag> <actor> <token> prints the HTTP status of
# the manifest. The registry's anonymous token endpoint is not enough for a
# private package, so the bearer token is requested with the given credentials.
ghcr_manifest_status() {
  local image="$1" tag="$2" actor="$3" token="$4" bearer
  bearer="$(curl -fsS -u "$actor:$token" \
    "https://ghcr.io/token?service=ghcr.io&scope=repository:${image#ghcr.io/}:pull" | jq -r .token)"
  curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $bearer" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/${image#ghcr.io/}/manifests/$tag"
}
