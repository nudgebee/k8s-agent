#!/usr/bin/env bash
# Publish packaged charts to the OCI registry, in addition to the classic Helm
# repo that chart-releaser maintains on gh-pages. Takes the tarball chart-releaser
# already built, so both channels serve the same bytes.
#
# Usage: publish-chart-oci.sh <chart.tgz>...
#
# Safe to re-run, and a re-run changes nothing:
# - A version already in the registry is left alone if it matches, and is a
#   hard failure if it does not.
# - A version is pushed only when the registry says it does not exist. Any
#   other failure to check (auth, rate limit, 5xx, network) fails the run,
#   because helm push would otherwise silently repoint the tag.
# - A digest that already carries a signature from this repo's workflows is not
#   signed again, so re-runs do not add Rekor entries.
#
# Requires helm and yq, plus cosign unless SKIP_SIGN=true. Also requires a prior
# `helm registry login`, and a docker login for cosign, which does not read
# Helm's credential store.
#
# CHART_REPO            target repo (default oci://ghcr.io/nudgebee/charts).
#                       The charts/ path is not cosmetic: the runner image
#                       already lives at ghcr.io/nudgebee/nudgebee-agent.
# HELM_OCI_FLAGS        extra flags for helm push/pull, e.g. --plain-http.
# SKIP_SIGN=true        skip the cosign signature.
# SIGN_IDENTITY_REGEXP  certificate identity an existing signature must have to
#                       count as already signed (default: any workflow of
#                       nudgebee/k8s-agent).
set -euo pipefail

CHART_REPO="${CHART_REPO:-oci://ghcr.io/nudgebee/charts}"
SIGN_IDENTITY_REGEXP="${SIGN_IDENTITY_REGEXP:-^https://github\.com/nudgebee/k8s-agent/\.github/workflows/}"
SIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"
read -r -a helm_flags <<<"${HELM_OCI_FLAGS:-}"

if [ "$#" -eq 0 ]; then
  echo "No chart packages to publish."
  exit 0
fi

# Check every tool up front, so a missing one fails before anything is pushed.
for tool in helm yq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "::error::$tool not found on PATH" >&2
    exit 1
  fi
done
if [ "${SKIP_SIGN:-false}" != "true" ] && ! command -v cosign >/dev/null 2>&1; then
  echo "::error::cosign not found on PATH. Install it, or set SKIP_SIGN=true to publish unsigned." >&2
  exit 1
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# Pulls name:version into dest and prints its digest. Returns 0 when pulled,
# 3 when the registry says the version does not exist (helm's error is not
# printed: that is the expected answer before a first push), and 1 on any other
# failure, after printing helm's error.
pull_chart() {
  local name="$1" version="$2" dest="$3" out
  if out="$(helm pull "$CHART_REPO/$name" --version "$version" --destination "$dest" \
    ${helm_flags[@]+"${helm_flags[@]}"} 2>&1)"; then
    sed -n 's/^Digest: //p' <<<"$out"
    return 0
  fi
  if grep -qiE ': not found$|manifest unknown|name unknown' <<<"$out"; then
    return 3
  fi
  echo "$out" >&2
  return 1
}

for pkg in "$@"; do
  chart="$(helm show chart "$pkg")"
  name="$(yq -r '.name' <<<"$chart")"
  version="$(yq -r '.version' <<<"$chart")"
  if [ -z "$name" ] || [ "$name" = null ] || [ -z "$version" ] || [ "$version" = null ]; then
    echo "::error::Could not read name and version from $pkg" >&2
    exit 1
  fi
  ref="$CHART_REPO/$name:$version"
  pulled="$workdir/$name-$version"
  mkdir -p "$pulled"

  echo "Checking whether $ref is already published"
  rc=0
  digest="$(pull_chart "$name" "$version" "$pulled")" || rc=$?
  case "$rc" in
    0)
      if ! cmp -s "$pkg" "$pulled/$name-$version.tgz"; then
        echo "::error::$ref already exists with different content than $pkg" >&2
        exit 1
      fi
      echo "$ref already published, unchanged"
      ;;
    3)
      echo "$ref is not published yet, pushing $pkg"
      helm push "$pkg" "$CHART_REPO" ${helm_flags[@]+"${helm_flags[@]}"}
      rc=0
      digest="$(pull_chart "$name" "$version" "$pulled")" || rc=$?
      if [ "$rc" -ne 0 ]; then
        echo "::error::$ref could not be pulled back after push" >&2
        exit 1
      fi
      if ! cmp -s "$pkg" "$pulled/$name-$version.tgz"; then
        echo "::error::$ref does not match $pkg after push" >&2
        exit 1
      fi
      ;;
    *)
      echo "::error::Could not check whether $ref exists; not pushing, since that could repoint an existing tag" >&2
      exit 1
      ;;
  esac

  if [ -z "$digest" ]; then
    echo "::error::helm pull reported no digest for $ref" >&2
    exit 1
  fi

  if [ "${SKIP_SIGN:-false}" != "true" ]; then
    image="${CHART_REPO#oci://}/$name@$digest"
    if cosign verify \
      --certificate-identity-regexp "$SIGN_IDENTITY_REGEXP" \
      --certificate-oidc-issuer "$SIGN_OIDC_ISSUER" \
      "$image" >/dev/null 2>&1; then
      echo "$ref ($digest) is already signed, not signing again"
    else
      echo "Signing $ref ($digest)"
      cosign sign --yes "$image"
    fi
  fi
done
