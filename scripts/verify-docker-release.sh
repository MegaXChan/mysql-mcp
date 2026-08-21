#!/usr/bin/env bash

# Verify the two immutable Docker aliases produced for one release tag.
# This runs before GitHub Release publication so a stale, partial, or malformed
# existing image can never be promoted into an official release.

set -Eeuo pipefail
export LC_ALL=C

fail() {
	printf 'verify-docker-release: %s\n' "$*" >&2
	exit 1
}

readonly SEMVER_PATTERN='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$'
readonly REVISION_PATTERN='^([0-9a-f]{40}|[0-9a-f]{64})$'
readonly DIGEST_PATTERN='^sha256:[0-9a-f]{64}$'

[[ -n "${IMAGE_NAME:-}" ]] || fail 'IMAGE_NAME is required'
[[ -n "${RELEASE_TAG:-}" ]] || fail 'RELEASE_TAG is required'
[[ "$RELEASE_TAG" =~ $SEMVER_PATTERN ]] || fail "RELEASE_TAG must be v-prefixed SemVer without build metadata: $RELEASE_TAG"
[[ -n "${EXPECTED_REVISION:-}" ]] || fail 'EXPECTED_REVISION is required'
[[ "$EXPECTED_REVISION" =~ $REVISION_PATTERN ]] || fail 'EXPECTED_REVISION must be a full Git SHA-1 or SHA-256 object ID'

for command_name in docker jq grep sort; do
	command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

readonly TEMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
readonly TEMP_PREFIX="${TEMP_BASE%/}/mysql-mcp-release-verify."
work_dir="$(mktemp -d "${TEMP_PREFIX}XXXXXXXX")" || fail 'could not create a temporary directory'
cleanup() {
	if [[ -n "${work_dir:-}" && "$work_dir" == "$TEMP_PREFIX"* && -d "$work_dir" ]]; then
		rm -rf "$work_dir"
	fi
}
trap cleanup EXIT

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
		printf 'could not verify Docker manifest %s (attempt %d/3)\n' "$image_ref" "$attempt" >&2
		if (( attempt < 3 )); then
			sleep "$attempt"
		fi
	done
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

first_digest=''
for image_tag in "$RELEASE_TAG" "${RELEASE_TAG#v}"; do
	image_ref="${IMAGE_NAME}:${image_tag}"
	manifest_file="$work_dir/manifest-${image_tag}.json"
	error_file="$work_dir/manifest-${image_tag}.error"
	inspect_required "$image_ref" "$manifest_file" "$error_file" || fail "release Docker tag is unavailable: $image_ref"
	digest="$(jq -r '.digest // ""' "$manifest_file")"
	revision="$(jq -r '.annotations["org.opencontainers.image.revision"] // ""' "$manifest_file")"
	version="$(jq -r '.annotations["org.opencontainers.image.version"] // ""' "$manifest_file")"
	[[ "$digest" =~ $DIGEST_PATTERN ]] || fail "release Docker tag has an invalid digest: $image_ref"
	[[ "$revision" == "$EXPECTED_REVISION" ]] || fail "release Docker tag revision does not match the Git tag: $image_ref"
	[[ "$version" == "$RELEASE_TAG" ]] || fail "release Docker tag version annotation is missing or incorrect: $image_ref"
	validate_runtime_platforms "$manifest_file" "$image_ref"
	if [[ -n "$first_digest" && "$digest" != "$first_digest" ]]; then
		fail "release Docker aliases do not point to one immutable manifest: $RELEASE_TAG"
	fi
	first_digest="$digest"
done

printf 'verified immutable Docker release aliases for %s (%s)\n' "$RELEASE_TAG" "$first_digest"
