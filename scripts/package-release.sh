#!/usr/bin/env bash

# Build one release target and package the files users need to run mysql-mcp.
#
# Inputs are deliberately passed through the environment so a CI matrix can
# invoke this script without constructing shell command lines from tag data:
#
#   VERSION=v1.2.3 COMMIT=0123456789abcdef GOOS=linux GOARCH=amd64 OUT_DIR=dist \
#     ./scripts/package-release.sh
#
# GOARM is required only for Linux/ARM and must be either 6 or 7. OUT_DIR may
# be relative (resolved from the caller's current directory) or absolute.

set -Eeuo pipefail

readonly PROGRAM_NAME="mysql-mcp"

fail() {
	printf 'package-release: %s\n' "$*" >&2
	exit 1
}

# Resolve repository paths from this script rather than relying on the caller's
# working directory. This keeps local, CI, and release-job behavior identical.
script_source="${BASH_SOURCE[0]}"
if [[ "$script_source" != /* ]]; then
	script_source="$PWD/$script_source"
fi
readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$script_source")" && pwd -P)"
readonly REPO_ROOT="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"

# A release version becomes part of an ldflag, directory name, and archive
# name. Requiring SemVer prevents both ambiguous assets and option/command
# injection. A conventional leading "v" is accepted for Git tags.
# Build metadata (+foo) is intentionally excluded. Docker tags cannot contain
# '+', and sanitizing it to '-' would collide with a real prerelease tag.
readonly SEMVER_PATTERN='^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$'

[[ -n "${VERSION:-}" ]] || fail 'VERSION is required (for example, v1.2.3)'
[[ -n "${GOOS:-}" ]] || fail 'GOOS is required'
[[ -n "${GOARCH:-}" ]] || fail 'GOARCH is required'
[[ -n "${OUT_DIR:-}" ]] || fail 'OUT_DIR is required'
[[ "$VERSION" =~ $SEMVER_PATTERN ]] || fail "VERSION must be a valid SemVer value: $VERSION"

# Prefer an explicitly supplied revision so release automation can identify the
# exact source checkout even when Git metadata is unavailable in the build
# environment. Local builds fall back to the repository HEAD.
if [[ -z "${COMMIT:-}" ]]; then
	COMMIT="$(git -C "$REPO_ROOT" rev-parse --verify HEAD 2>/dev/null || printf '%s' unknown)"
fi
readonly COMMIT
readonly COMMIT_PATTERN='^(unknown|[0-9A-Fa-f]{7,64})$'
[[ "$COMMIT" =~ $COMMIT_PATTERN ]] || fail 'COMMIT must be "unknown" or a 7-64 character hexadecimal Git object ID'

# Keep this allowlist synchronized with the targets published by the release
# workflow. Unsupported OS/CPU combinations fail before downloading modules or
# producing a partial archive.
target_arch="$GOARCH"
case "$GOOS/$GOARCH" in
	linux/386 | linux/amd64 | linux/arm64 | windows/386 | windows/amd64 | windows/arm64 | darwin/amd64 | darwin/arm64)
		[[ -z "${GOARM:-}" ]] || fail "GOARM is only valid when GOOS=linux and GOARCH=arm"
		;;
	linux/arm)
		case "${GOARM:-}" in
			6 | 7)
				target_arch="armv${GOARM}"
				;;
			*)
				fail 'Linux/ARM builds require GOARM=6 or GOARM=7'
				;;
		esac
		;;
	*)
		fail "unsupported release target: $GOOS/$GOARCH"
		;;
esac

command -v "${GO:-go}" >/dev/null 2>&1 || fail "Go executable not found: ${GO:-go}"
if [[ "$GOOS" == "windows" ]]; then
	command -v zip >/dev/null 2>&1 || fail 'zip is required to package Windows releases'
else
	command -v tar >/dev/null 2>&1 || fail 'tar is required to package Unix releases'
fi

for required_path in \
	"$REPO_ROOT/go.mod" \
	"$REPO_ROOT/README.md" \
	"$REPO_ROOT/README.zh-CN.md" \
	"$REPO_ROOT/config.example.yaml" \
	"$REPO_ROOT/docs"; do
	[[ -e "$required_path" ]] || fail "required release content is missing: $required_path"
done

output_request="$OUT_DIR"
if [[ "$output_request" != /* ]]; then
	output_request="$PWD/$output_request"
fi
mkdir -p "$output_request"
readonly OUTPUT_DIR="$(CDPATH= cd -- "$output_request" && pwd -P)"

readonly TARGET_NAME="${GOOS}-${target_arch}"
readonly PACKAGE_NAME="${PROGRAM_NAME}_${VERSION}_${TARGET_NAME}"
if [[ "$GOOS" == "windows" ]]; then
	readonly BINARY_NAME="${PROGRAM_NAME}.exe"
	readonly ARCHIVE_NAME="${PACKAGE_NAME}.zip"
else
	readonly BINARY_NAME="$PROGRAM_NAME"
	readonly ARCHIVE_NAME="${PACKAGE_NAME}.tar.gz"
fi
readonly FINAL_ARCHIVE="$OUTPUT_DIR/$ARCHIVE_NAME"

# Refusing to overwrite an existing asset avoids silently replacing a release
# with output built from a different commit. CI jobs should start with an empty
# output directory, and local callers can explicitly remove an obsolete asset.
[[ ! -e "$FINAL_ARCHIVE" ]] || fail "release archive already exists: $FINAL_ARCHIVE"

# All intermediate files live below one uniquely created directory. The prefix
# guard in cleanup ensures an unset or corrupted variable can never broaden the
# recursive removal target.
readonly TEMP_BASE="${TMPDIR:-/tmp}"
readonly TEMP_PREFIX="${TEMP_BASE%/}/${PROGRAM_NAME}-release."
temp_root="$(mktemp -d "${TEMP_PREFIX}XXXXXXXX")" || fail 'could not create a temporary directory'
cleanup() {
	if [[ -n "${temp_root:-}" && "$temp_root" == "$TEMP_PREFIX"* && -d "$temp_root" ]]; then
		rm -rf "$temp_root"
	fi
}
trap cleanup EXIT

readonly STAGING_DIR="$temp_root/$PACKAGE_NAME"
mkdir -p "$STAGING_DIR/docs"

# CGO is disabled so every produced executable is self-contained and can be
# cross-compiled from the Linux GitHub Actions runner. -trimpath removes local
# checkout paths; -buildvcs=false prevents runner-specific VCS metadata; and
# main.version is set from the validated release tag, and main.commit identifies
# the exact source revision used to produce the archive.
build_env=(
	"CGO_ENABLED=0"
	"GOTOOLCHAIN=local"
	"GOOS=$GOOS"
	"GOARCH=$GOARCH"
)
if [[ "$GOARCH" == "arm" ]]; then
	build_env+=("GOARM=$GOARM")
fi

(
	cd -- "$REPO_ROOT"
	env "${build_env[@]}" "${GO:-go}" build \
		-mod=readonly \
		-trimpath \
		-buildvcs=false \
		-ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" \
		-o "$STAGING_DIR/$BINARY_NAME" \
		./cmd/mysql-mcp
)

# Copy only user-facing runtime material. Keeping a single top-level directory
# in every archive prevents extraction from scattering files into the current
# working directory.
cp "$REPO_ROOT/README.md" "$STAGING_DIR/README.md"
cp "$REPO_ROOT/README.zh-CN.md" "$STAGING_DIR/README.zh-CN.md"
cp "$REPO_ROOT/config.example.yaml" "$STAGING_DIR/config.example.yaml"
cp -R "$REPO_ROOT/docs/." "$STAGING_DIR/docs/"

readonly TEMP_ARCHIVE="$temp_root/$ARCHIVE_NAME"
if [[ "$GOOS" == "windows" ]]; then
	(
		cd -- "$temp_root"
		# Info-ZIP treats -- before the archive name as an error. Both names
		# have a fixed mysql-mcp prefix and cannot be interpreted as options.
		zip -q -r "$ARCHIVE_NAME" "$PACKAGE_NAME"
	)
else
	# COPYFILE_DISABLE prevents macOS tar from adding AppleDouble metadata when
	# a maintainer builds an archive locally on macOS.
	COPYFILE_DISABLE=1 tar -C "$temp_root" -czf "$TEMP_ARCHIVE" "$PACKAGE_NAME"
fi

[[ -s "$TEMP_ARCHIVE" ]] || fail 'packaging produced an empty archive'
mv "$TEMP_ARCHIVE" "$FINAL_ARCHIVE"

printf '%s\n' "$FINAL_ARCHIVE"
