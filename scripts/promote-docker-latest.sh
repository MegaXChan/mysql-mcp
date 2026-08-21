#!/usr/bin/env bash

# Reconcile IMAGE_NAME:latest to the highest stable GitHub Release.
#
# This script deliberately does not trust workflow arrival order or the current
# release tag. It derives the global highest stable SemVer from GitHub Releases,
# verifies both immutable Docker aliases for that release, and only then moves
# latest by exact digest. All callers must hold the shared Docker-latest
# concurrency lock configured in the publish workflow.

set -Eeuo pipefail
export LC_ALL=C

fail() {
	printf 'promote-docker-latest: %s\n' "$*" >&2
	exit 1
}

readonly STABLE_SEMVER_PATTERN='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
readonly GIT_REVISION_PATTERN='^([0-9a-f]{40}|[0-9a-f]{64})$'
readonly DIGEST_PATTERN='^sha256:[0-9a-f]{64}$'

[[ -n "${IMAGE_NAME:-}" ]] || fail 'IMAGE_NAME is required'
[[ -n "${GITHUB_REPOSITORY:-}" ]] || fail 'GITHUB_REPOSITORY is required'
[[ -n "${RELEASE_TAG:-}" ]] || fail 'RELEASE_TAG is required'
[[ "$RELEASE_TAG" =~ $STABLE_SEMVER_PATTERN ]] || fail "RELEASE_TAG must be stable v-prefixed SemVer: $RELEASE_TAG"
[[ -n "${GH_TOKEN:-}" ]] || fail 'GH_TOKEN is required'

for command_name in docker gh git jq grep sort tr; do
	command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

readonly TEMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
readonly TEMP_PREFIX="${TEMP_BASE%/}/mysql-mcp-latest."
work_dir="$(mktemp -d "${TEMP_PREFIX}XXXXXXXX")" || fail 'could not create a temporary directory'
cleanup() {
	if [[ -n "${work_dir:-}" && "$work_dir" == "$TEMP_PREFIX"* && -d "$work_dir" ]]; then
		rm -rf "$work_dir"
	fi
}
trap cleanup EXIT

# Return 1, 0, or -1 when the left stable version is greater than, equal to,
# or less than the right version. Length-first comparison avoids fixed-width
# integer arithmetic and remains correct for arbitrarily large SemVer numbers.
compare_stable_versions() {
	local left="${1#v}"
	local right="${2#v}"
	local -a left_parts right_parts
	local index left_part right_part
	IFS='.' read -r -a left_parts <<< "$left"
	IFS='.' read -r -a right_parts <<< "$right"
	for index in 0 1 2; do
		left_part="${left_parts[index]}"
		right_part="${right_parts[index]}"
		if (( ${#left_part} > ${#right_part} )); then
			printf '1\n'
			return
		fi
		if (( ${#left_part} < ${#right_part} )); then
			printf '%s\n' '-1'
			return
		fi
		if [[ "$left_part" > "$right_part" ]]; then
			printf '1\n'
			return
		fi
		if [[ "$left_part" < "$right_part" ]]; then
			printf '%s\n' '-1'
			return
		fi
	done
	printf '0\n'
}

is_official_stable_release() {
	local candidate="$1"
	local published_tag
	while IFS= read -r published_tag; do
		[[ "$published_tag" == "$candidate" ]] && return 0
	done <<< "$stable_release_tags"
	return 1
}

# GitHub's release list is the source of truth for "published". Retry both API
# failures and short-lived post-create visibility lag; never infer a release
# from a Git tag or Docker tag alone.
release_rows=''
release_error="$work_dir/releases-error.txt"
release_list_complete=false
for attempt in 1 2 3; do
	candidate_rows=''
	if candidate_rows="$(gh api --paginate \
		"repos/${GITHUB_REPOSITORY}/releases?per_page=100" \
		--jq '.[] | select(.draft == false and .prerelease == false) | .tag_name' \
		2>"$release_error")"; then
		if grep -Fxq -- "$RELEASE_TAG" <<< "$candidate_rows"; then
			release_rows="$candidate_rows"
			release_list_complete=true
			break
		fi
		printf 'stable release %s is not visible yet (attempt %d/3)\n' "$RELEASE_TAG" "$attempt" >&2
	else
		printf 'could not list GitHub Releases (attempt %d/3)\n' "$attempt" >&2
	fi
	if (( attempt < 3 )); then
		sleep "$attempt"
	fi
done
if [[ "$release_list_complete" != true ]]; then
	if [[ -s "$release_error" ]]; then
		cat "$release_error" >&2
	fi
	fail "the completed stable release is not available from the GitHub API: $RELEASE_TAG"
fi

stable_release_tags=''
highest_tag=''
while IFS= read -r candidate; do
	[[ "$candidate" =~ $STABLE_SEMVER_PATTERN ]] || continue
	stable_release_tags+="${candidate}"$'\n'
	if [[ -z "$highest_tag" || "$(compare_stable_versions "$candidate" "$highest_tag")" == '1' ]]; then
		highest_tag="$candidate"
	fi
done <<< "$release_rows"
[[ -n "$highest_tag" ]] || fail 'GitHub has no stable v-prefixed SemVer Release'
readonly stable_release_tags highest_tag

inspect_required() {
	local image_ref="$1"
	local output_file="$2"
	local error_file="$3"
	local attempt
	for attempt in 1 2 3; do
		if docker buildx imagetools inspect --format '{{json .Manifest}}' "$image_ref" >"$output_file" 2>"$error_file" &&
			jq -e 'type == "object" and
				(.digest | type == "string") and
				(.annotations | type == "object") and
				(.manifests | type == "array" and length > 0)' "$output_file" >/dev/null; then
			return 0
		fi
		printf 'could not verify required Docker manifest %s (attempt %d/3)\n' "$image_ref" "$attempt" >&2
		if (( attempt < 3 )); then
			sleep "$attempt"
		fi
	done
	[[ ! -s "$error_file" ]] || cat "$error_file" >&2
	return 1
}

# Return 0 when latest exists, 2 only after three explicit not-found results,
# and 1 for every ambiguous registry, network, authentication, or JSON error.
inspect_optional_latest() {
	local image_ref="$1"
	local output_file="$2"
	local error_file="$3"
	local attempt inspect_error lower_error lower_ref
	local not_found_count=0
	lower_ref="$(printf '%s' "$image_ref" | tr '[:upper:]' '[:lower:]')"
	for attempt in 1 2 3; do
		if docker buildx imagetools inspect --format '{{json .Manifest}}' "$image_ref" >"$output_file" 2>"$error_file"; then
			if jq -e 'type == "object" and
				(.digest | type == "string") and
				(.annotations | type == "object") and
				(.manifests | type == "array" and length > 0)' "$output_file" >/dev/null; then
				return 0
			fi
			printf 'Docker returned an invalid manifest for %s (attempt %d/3)\n' "$image_ref" "$attempt" >&2
		else
			printf 'could not inspect optional Docker manifest %s (attempt %d/3)\n' "$image_ref" "$attempt" >&2
			inspect_error="$(<"$error_file")"
			lower_error="$(printf '%s' "$inspect_error" | tr '[:upper:]' '[:lower:]')"
			if [[ "$lower_error" == *"${lower_ref}: not found"* ||
				"$lower_error" == *"${lower_ref}: manifest unknown"* ||
				"$lower_error" == *"${lower_ref}: no such manifest"* ||
				"$lower_error" == *"${lower_ref}: manifest not found"* ||
				"$lower_error" == *"${lower_ref} not found"* ]]; then
				not_found_count=$((not_found_count + 1))
			fi
		fi
		if (( attempt < 3 )); then
			sleep "$attempt"
		fi
	done

	if (( not_found_count == 3 )); then
		return 2
	fi
	[[ ! -s "$error_file" ]] || cat "$error_file" >&2
	return 1
}

validate_runtime_platforms() {
	local manifest_file="$1"
	local image_ref="$2"
	local platforms required
	platforms="$(jq -r '
		.manifests[]? |
		select(.platform.os == "linux") |
		if .platform.architecture == "arm" then
			"linux/arm/" + (.platform.variant // "")
		elif .platform.architecture == "arm64" then
			"linux/arm64"
		else
			"linux/" + (.platform.architecture // "")
		end
	' "$manifest_file" | sort -u)"
	for required in linux/386 linux/amd64 linux/arm/v6 linux/arm/v7 linux/arm64; do
		grep -Fxq -- "$required" <<< "$platforms" || fail "Docker image is missing runtime platform $required: $image_ref"
	done
}

# Validate both v-prefixed and unprefixed immutable aliases. The function sets
# VALIDATED_DIGEST and VALIDATED_REVISION for its caller.
validate_exact_release() {
	local release_tag="$1"
	local expected_revision image_tag image_ref manifest_file error_file
	local digest revision version first_digest=''
	expected_revision="$(git rev-parse --verify "${release_tag}^{commit}" 2>/dev/null)" || fail "cannot resolve Git tag to a commit: $release_tag"
	[[ "$expected_revision" =~ $GIT_REVISION_PATTERN ]] || fail "Git tag resolved to an invalid revision: $release_tag"

	for image_tag in "$release_tag" "${release_tag#v}"; do
		image_ref="${IMAGE_NAME}:${image_tag}"
		manifest_file="$work_dir/exact-${image_tag}.json"
		error_file="$work_dir/exact-${image_tag}.error"
		inspect_required "$image_ref" "$manifest_file" "$error_file" || fail "required exact Docker tag is unavailable: $image_ref"
		digest="$(jq -r '.digest // ""' "$manifest_file")"
		revision="$(jq -r '.annotations["org.opencontainers.image.revision"] // ""' "$manifest_file")"
		version="$(jq -r '.annotations["org.opencontainers.image.version"] // ""' "$manifest_file")"
		[[ "$digest" =~ $DIGEST_PATTERN ]] || fail "exact Docker tag has an invalid digest: $image_ref"
		[[ "$revision" == "$expected_revision" ]] || fail "exact Docker tag revision does not match Git: $image_ref"
		[[ "$version" == "$release_tag" ]] || fail "exact Docker tag version annotation is missing or incorrect: $image_ref"
		validate_runtime_platforms "$manifest_file" "$image_ref"
		if [[ -n "$first_digest" && "$digest" != "$first_digest" ]]; then
			fail "exact Docker aliases do not share one immutable manifest: $release_tag"
		fi
		first_digest="$digest"
	done

	VALIDATED_DIGEST="$first_digest"
	VALIDATED_REVISION="$expected_revision"
}

validate_exact_release "$highest_tag"
highest_digest="$VALIDATED_DIGEST"
highest_revision="$VALIDATED_REVISION"
readonly highest_digest highest_revision

latest_ref="${IMAGE_NAME}:latest"
latest_manifest="$work_dir/latest.json"
latest_error="$work_dir/latest.error"
latest_exists=false
if inspect_optional_latest "$latest_ref" "$latest_manifest" "$latest_error"; then
	latest_exists=true
else
	inspect_status=$?
	if (( inspect_status != 2 )); then
		fail "could not safely determine the state of $latest_ref"
	fi
fi

if [[ "$latest_exists" == true ]]; then
	published_digest="$(jq -r '.digest // ""' "$latest_manifest")"
	published_revision="$(jq -r '.annotations["org.opencontainers.image.revision"] // ""' "$latest_manifest")"
	published_version="$(jq -r '.annotations["org.opencontainers.image.version"] // ""' "$latest_manifest")"
	[[ "$published_digest" =~ $DIGEST_PATTERN ]] || fail "latest has an invalid manifest digest: $latest_ref"
	[[ "$published_revision" =~ $GIT_REVISION_PATTERN ]] || fail "latest has an invalid revision annotation: $latest_ref"
	[[ "$published_version" =~ $STABLE_SEMVER_PATTERN ]] || fail "latest has an invalid stable-version annotation: $latest_ref"
	is_official_stable_release "$published_version" || fail "latest does not correspond to an official stable GitHub Release: $published_version"

	comparison="$(compare_stable_versions "$highest_tag" "$published_version")"
	if [[ "$comparison" == '-1' ]]; then
		fail "latest is newer than the highest official stable Release; refusing to overwrite it"
	fi
	if [[ "$comparison" == '0' ]]; then
		[[ "$published_digest" == "$highest_digest" && "$published_revision" == "$highest_revision" ]] ||
			fail 'latest version matches the highest Release but its immutable image does not'
		printf 'latest already points to highest stable release %s (%s)\n' "$highest_tag" "$highest_digest"
		exit 0
	fi
fi

# A single existing image index is carbon-copied by digest. Because the exact
# manifest already contains the trusted version and revision annotations, the
# resulting latest tag must resolve to the identical root digest.
docker buildx imagetools create --tag "$latest_ref" "${IMAGE_NAME}@${highest_digest}"
inspect_required "$latest_ref" "$latest_manifest" "$latest_error" || fail "could not verify updated latest tag: $latest_ref"
updated_digest="$(jq -r '.digest // ""' "$latest_manifest")"
updated_revision="$(jq -r '.annotations["org.opencontainers.image.revision"] // ""' "$latest_manifest")"
updated_version="$(jq -r '.annotations["org.opencontainers.image.version"] // ""' "$latest_manifest")"
[[ "$updated_digest" == "$highest_digest" ]] || fail 'latest digest differs from the highest exact release after promotion'
[[ "$updated_revision" == "$highest_revision" ]] || fail 'latest revision differs from the highest exact release after promotion'
[[ "$updated_version" == "$highest_tag" ]] || fail 'latest version differs from the highest exact release after promotion'
printf 'advanced %s to highest stable release %s (%s)\n' "$latest_ref" "$highest_tag" "$highest_digest"
