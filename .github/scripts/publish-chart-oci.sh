#!/usr/bin/env bash
# Publish packaged charts to the OCI registry, in addition to the classic Helm
# repo that chart-releaser maintains on gh-pages. Takes the tarball chart-releaser
# already built, so both channels serve the same bytes.
#
# Usage: publish-chart-oci.sh <chart.tgz>...
#
# Safe to re-run: a version already in the registry is left alone if it matches
# and is a hard failure if it does not, because helm push would otherwise
# silently repoint the tag.
#
# Requires a prior `helm registry login`, plus a docker login for cosign, which
# does not read Helm's credential store.
#
# CHART_REPO       target repo (default oci://ghcr.io/nudgebee/charts). The
#                  charts/ path is not cosmetic: the runner image already lives
#                  at ghcr.io/nudgebee/nudgebee-agent.
# HELM_OCI_FLAGS   extra flags for helm push/pull, e.g. --plain-http.
# SKIP_SIGN=true   skip the cosign signature.
set -euo pipefail

CHART_REPO="${CHART_REPO:-oci://ghcr.io/nudgebee/charts}"
read -r -a helm_flags <<<"${HELM_OCI_FLAGS:-}"

if [ "$#" -eq 0 ]; then
  echo "No chart packages to publish."
  exit 0
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# Pulls name:version into dest and prints its digest. Fails if it is not there.
pull_chart() {
  local name="$1" version="$2" dest="$3" out
  out="$(helm pull "$CHART_REPO/$name" --version "$version" --destination "$dest" \
    ${helm_flags[@]+"${helm_flags[@]}"} 2>&1)" || {
    echo "$out" >&2
    return 1
  }
  sed -n 's/^Digest: //p' <<<"$out"
}

for pkg in "$@"; do
  name="$(helm show chart "$pkg" | sed -n 's/^name: //p')"
  version="$(helm show chart "$pkg" | sed -n 's/^version: //p')"
  ref="$CHART_REPO/$name:$version"
  pulled="$workdir/$name-$version"
  mkdir -p "$pulled"

  if digest="$(pull_chart "$name" "$version" "$pulled")"; then
    if ! cmp -s "$pkg" "$pulled/$name-$version.tgz"; then
      echo "::error::$ref already exists with different content than $pkg" >&2
      exit 1
    fi
    echo "$ref already published, unchanged"
  else
    echo "Pushing $pkg to $CHART_REPO"
    helm push "$pkg" "$CHART_REPO" ${helm_flags[@]+"${helm_flags[@]}"}
    digest="$(pull_chart "$name" "$version" "$pulled")"
    if ! cmp -s "$pkg" "$pulled/$name-$version.tgz"; then
      echo "::error::$ref does not match $pkg after push" >&2
      exit 1
    fi
  fi

  if [ "${SKIP_SIGN:-false}" != "true" ]; then
    echo "Signing $ref ($digest)"
    cosign sign --yes "${CHART_REPO#oci://}/$name@$digest"
  fi
done
