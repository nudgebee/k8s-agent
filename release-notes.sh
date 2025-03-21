#!/bin/bash

set -e

#GITHUB_TOKEN=""
REGION="us-east-1"

# Define ECR repositories and their corresponding paths in values.yaml
ECR_REPOS=(
  "nudgebee-agent:.runner.image.tag"
  "nudgebee-node-agent:.nodeAgent.image.tag"
  "nudgebee-profiler-python:.runner.profiler_image_override"
  "krr-public:.runner.krr_image_override"
  "nova:.runner.nova_image_override"
  "kubewatch:.kubewatch.image.tag"
  "nudgebee_runbook_sidecar_agent:.runner.runbook_sidecar_image_tag"
)

# Matching GitHub repositories (index-based mapping)
GITHUB_REPOS=(
  "nudgebee/robusta"
  "nudgebee/node-agent"
  "nudgebee/krr"
  "nudgebee/nova"
  "nudgebee/kubewatch"
  "nudgebee/nudgebee-runbook-sidecar"
)

echo "Fetching latest image tags from ECR..."

IMAGE_REPOS=()
IMAGE_TAGS=()

for repo_path in "${ECR_REPOS[@]}"; do
  repo="${repo_path%%:*}"
  tag=$(aws ecr describe-images --repository-name "$repo" --query 'sort_by(imageDetails[?imageTags != `null`],& imagePushedAt)[-1].imageTags[0]' --region "$REGION" --output text --no-paginate)

  if [[ "$tag" != "None" ]]; then
    IMAGE_REPOS+=("$repo")
    IMAGE_TAGS+=("$tag")
  fi
done

if [[ ${#IMAGE_REPOS[@]} -eq 0 ]]; then
  echo "No new images detected."
  exit 0
fi

echo "Fetching GitHub commit history for changed images..."

RELEASE_NOTES="### Release Notes\n\n"

for i in "${!IMAGE_REPOS[@]}"; do
  repo="${IMAGE_REPOS[$i]}"
  new_tag="${IMAGE_TAGS[$i]}"

  # Extract commit hash from tag
  COMMIT_HASH=$(echo "$new_tag" | awk -F'_' '{print $2}')

  # Find the corresponding GitHub repo using index
  for j in "${!ECR_REPOS[@]}"; do
    if [[ "$repo" == "${ECR_REPOS[$j]%%:*}" ]]; then
      GITHUB_REPO="${GITHUB_REPOS[$j]}"
      break
    fi
  done

  if [[ -z "$GITHUB_REPO" ]]; then
    echo "No GitHub repo found for $repo, skipping..."
    continue
  fi

  echo "Fetching commits for $GITHUB_REPO..."

  echo "curl -s -H \"Authorization: token <REDACTED>\" \
                     -H \"Accept: application/vnd.github.v3+json\" \
                     \"https://api.github.com/repos/$GITHUB_REPO/commits?sha=$COMMIT_HASH&per_page=5\""
                     
  # Get commit history using GitHub API
  API_RESPONSE=$(curl -s -H "Authorization: token $GITHUB_TOKEN" \
                     -H "Accept: application/vnd.github.v3+json" \
                     "https://api.github.com/repos/$GITHUB_REPO/commits?sha=$COMMIT_HASH&per_page=5")

  # Debug: Check if API response is valid JSON
  if ! echo "$API_RESPONSE" | jq empty 2>/dev/null; then
    echo "Error: API response is not valid JSON"
    echo "$API_RESPONSE"
    continue
  fi

  # Ensure response is an array
  if [[ $(echo "$API_RESPONSE" | jq 'if type=="array" then "OK" else "ERROR" end') != "\"OK\"" ]]; then
    echo "Error: API response is not a commit list"
    echo "$API_RESPONSE"
    continue
  fi

  # Extract commit messages
  COMMIT_MESSAGES=$(echo "$API_RESPONSE" | jq -r '.[] | "- \(.commit.message) (by \(.commit.author.name))"')

  if [[ -n "$COMMIT_MESSAGES" ]]; then
    RELEASE_NOTES+="#### $repo ($GITHUB_REPO)\n"
    RELEASE_NOTES+="$COMMIT_MESSAGES\n\n"
  fi
done

# Save release notes
echo -e "$RELEASE_NOTES"
echo -e "$RELEASE_NOTES" > release_notes.txt
echo "Release notes saved to release_notes.txt"

exit 0
