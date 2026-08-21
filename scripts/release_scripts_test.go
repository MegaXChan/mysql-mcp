package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	testV1Revision = "1111111111111111111111111111111111111111"
	testV2Revision = "2222222222222222222222222222222222222222"
)

// TestPromoteDockerLatestStateMachine locks down the failure and recovery
// boundaries that cannot be exercised against Docker Hub from pull-request CI.
// Fake commands model registry state while the real Bash script, jq filters,
// SemVer comparison, retry logic, and fail-closed decisions execute unchanged.
func TestPromoteDockerLatestStateMachine(t *testing.T) {
	requireReleaseScriptTools(t)

	tests := []struct {
		name        string
		triggerTag  string
		releaseTags string
		githubMode  string
		latestState string
		exactMode   string
		wantSuccess bool
		wantLatest  string
		wantCreates int
	}{
		{
			name:        "missing latest is rebuilt from global highest release",
			triggerTag:  "v1.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "missing",
			wantSuccess: true,
			wantLatest:  "v2.0.0",
			wantCreates: 1,
		},
		{
			name:        "older latest advances to global highest release",
			triggerTag:  "v1.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "v1.0.0",
			exactMode:   "bad-v1-version",
			wantSuccess: true,
			wantLatest:  "v2.0.0",
			wantCreates: 1,
		},
		{
			name:        "highest latest is an idempotent no-op",
			triggerTag:  "v1.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "v2.0.0",
			wantSuccess: true,
			wantLatest:  "v2.0.0",
			wantCreates: 0,
		},
		{
			name:        "partial paginated release list fails closed",
			triggerTag:  "v1.0.0",
			releaseTags: "v1.0.0\n",
			githubMode:  "partial-failure",
			latestState: "missing",
			wantSuccess: false,
			wantLatest:  "missing",
			wantCreates: 0,
		},
		{
			name:        "mixed not-found and network failures are not absence",
			triggerTag:  "v2.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "mixed-error",
			wantSuccess: false,
			wantLatest:  "mixed-error",
			wantCreates: 0,
		},
		{
			name:        "generic manifest unknown is not target-specific absence",
			triggerTag:  "v2.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "ambiguous-not-found",
			wantSuccess: false,
			wantLatest:  "ambiguous-not-found",
			wantCreates: 0,
		},
		{
			name:        "target-naming authentication proxy 404 is not absence",
			triggerTag:  "v2.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "auth-proxy-404",
			wantSuccess: false,
			wantLatest:  "auth-proxy-404",
			wantCreates: 0,
		},
		{
			name:        "same-version digest mismatch fails closed",
			triggerTag:  "v2.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "tampered-v2",
			wantSuccess: false,
			wantLatest:  "tampered-v2",
			wantCreates: 0,
		},
		{
			name:        "unofficial higher latest fails closed",
			triggerTag:  "v2.0.0",
			releaseTags: "v1.0.0\nv2.0.0\n",
			githubMode:  "success",
			latestState: "v3.0.0",
			wantSuccess: false,
			wantLatest:  "v3.0.0",
			wantCreates: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runReleaseScript(t, "promote-docker-latest.sh", releaseScriptOptions{
				triggerTag:  test.triggerTag,
				releaseTags: test.releaseTags,
				githubMode:  test.githubMode,
				latestState: test.latestState,
				exactMode:   test.exactMode,
			})
			if (result.err == nil) != test.wantSuccess {
				t.Fatalf("success = %v, want %v; error=%v\noutput:\n%s", result.err == nil, test.wantSuccess, result.err, result.output)
			}
			if result.latestState != test.wantLatest {
				t.Fatalf("latest state = %q, want %q; output:\n%s", result.latestState, test.wantLatest, result.output)
			}
			if result.createCount != test.wantCreates {
				t.Fatalf("create count = %d, want %d; output:\n%s", result.createCount, test.wantCreates, result.output)
			}
		})
	}
}

// TestVerifyDockerRelease rejects stale aliases before the GitHub Release job
// can publish them as official assets.
func TestVerifyDockerRelease(t *testing.T) {
	requireReleaseScriptTools(t)

	for _, test := range []struct {
		name        string
		exactMode   string
		wantSuccess bool
	}{
		{name: "valid aliases", exactMode: "valid", wantSuccess: true},
		{name: "wrong version annotation", exactMode: "bad-v2-version", wantSuccess: false},
		{name: "missing runtime platform", exactMode: "missing-platform", wantSuccess: false},
		{name: "aliases have different digests", exactMode: "split-digest", wantSuccess: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runReleaseScript(t, "verify-docker-release.sh", releaseScriptOptions{
				triggerTag:  "v2.0.0",
				releaseTags: "v2.0.0\n",
				githubMode:  "success",
				latestState: "missing",
				exactMode:   test.exactMode,
			})
			if (result.err == nil) != test.wantSuccess {
				t.Fatalf("success = %v, want %v; error=%v\noutput:\n%s", result.err == nil, test.wantSuccess, result.err, result.output)
			}
		})
	}
}

// TestPackageReleaseArchivesAreReproducible uses a deterministic fake compiler
// to isolate archive metadata. Repeating the same tag/revision build must yield
// identical tar.gz and ZIP bytes, which makes immutable Release verification
// possible after a runner or network failure.
func TestPackageReleaseArchivesAreReproducible(t *testing.T) {
	requireReleaseScriptTools(t)
	for _, command := range []string{"tar", "gzip", "zip"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("reproducibility test requires %s", command)
		}
	}

	compilerDir := t.TempDir()
	fakeGo := filepath.Join(compilerDir, "fake-go")
	writeFakeTool(t, compilerDir, "fake-go", fakeGoCompiler)
	scriptPath, err := filepath.Abs("package-release.sh")
	if err != nil {
		t.Fatalf("resolve package script path: %v", err)
	}

	for _, target := range []struct {
		name string
		goos string
		ext  string
	}{
		{name: "linux-amd64", goos: "linux", ext: "tar.gz"},
		{name: "windows-amd64", goos: "windows", ext: "zip"},
	} {
		t.Run(target.name, func(t *testing.T) {
			root := t.TempDir()
			first := filepath.Join(root, "first")
			second := filepath.Join(root, "second")
			if err := os.Mkdir(first, 0o700); err != nil {
				t.Fatalf("create first output: %v", err)
			}
			if err := os.Mkdir(second, 0o700); err != nil {
				t.Fatalf("create second output: %v", err)
			}
			for _, outputDirectory := range []string{first, second} {
				command := exec.Command("bash", scriptPath)
				command.Env = append(os.Environ(),
					"GO="+fakeGo,
					"VERSION=v1.2.3",
					"COMMIT="+testV2Revision,
					"GOOS="+target.goos,
					"GOARCH=amd64",
					"GOARM=",
					"OUT_DIR="+outputDirectory,
				)
				if output, runErr := command.CombinedOutput(); runErr != nil {
					t.Fatalf("package release into %s: %v\n%s", outputDirectory, runErr, output)
				}
			}
			archiveName := "mysql-mcp_v1.2.3_" + target.name + "." + target.ext
			firstBytes, err := os.ReadFile(filepath.Join(first, archiveName))
			if err != nil {
				t.Fatalf("read first archive: %v", err)
			}
			secondBytes, err := os.ReadFile(filepath.Join(second, archiveName))
			if err != nil {
				t.Fatalf("read second archive: %v", err)
			}
			if !bytes.Equal(firstBytes, secondBytes) {
				t.Fatalf("repeated %s archives are not byte-for-byte reproducible", target.name)
			}
		})
	}
}

type releaseScriptOptions struct {
	triggerTag  string
	releaseTags string
	githubMode  string
	latestState string
	exactMode   string
}

type releaseScriptResult struct {
	output      string
	err         error
	latestState string
	createCount int
}

func runReleaseScript(t *testing.T, scriptName string, options releaseScriptOptions) releaseScriptResult {
	t.Helper()

	stateDir := t.TempDir()
	binDir := filepath.Join(stateDir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create fake bin directory: %v", err)
	}
	writeFakeTool(t, binDir, "gh", fakeGitHubCLI)
	writeFakeTool(t, binDir, "git", fakeGitCLI)
	writeFakeTool(t, binDir, "docker", fakeDockerCLI)
	writeFakeTool(t, binDir, "sleep", "#!/usr/bin/env bash\nexit 0\n")

	if err := os.WriteFile(filepath.Join(stateDir, "latest-state"), []byte(options.latestState+"\n"), 0o600); err != nil {
		t.Fatalf("write latest state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "create-count"), []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write create count: %v", err)
	}

	scriptPath, err := filepath.Abs(scriptName)
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	command := exec.Command("bash", scriptPath)
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RUNNER_TEMP="+stateDir,
		"FAKE_STATE_DIR="+stateDir,
		"FAKE_RELEASE_TAGS="+options.releaseTags,
		"FAKE_GH_MODE="+options.githubMode,
		"FAKE_EXACT_MODE="+options.exactMode,
		"IMAGE_NAME=megaxcn/mysql-mcp",
		"GITHUB_REPOSITORY=MegaXChan/mysql-mcp",
		"GH_TOKEN=test-token",
		"RELEASE_TAG="+options.triggerTag,
		"EXPECTED_REVISION="+revisionForTag(options.triggerTag),
	)
	output, runErr := command.CombinedOutput()
	latestBytes, readErr := os.ReadFile(filepath.Join(stateDir, "latest-state"))
	if readErr != nil {
		t.Fatalf("read latest state: %v", readErr)
	}
	countBytes, readErr := os.ReadFile(filepath.Join(stateDir, "create-count"))
	if readErr != nil {
		t.Fatalf("read create count: %v", readErr)
	}
	createCount, parseErr := strconv.Atoi(strings.TrimSpace(string(countBytes)))
	if parseErr != nil {
		t.Fatalf("parse create count: %v", parseErr)
	}
	return releaseScriptResult{
		output:      string(output),
		err:         runErr,
		latestState: strings.TrimSpace(string(latestBytes)),
		createCount: createCount,
	}
}

func requireReleaseScriptTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release scripts run on the Linux publishing runner")
	}
	for _, command := range []string{"bash", "jq", "grep", "sort", "tr"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("release script test requires %s", command)
		}
	}
}

func writeFakeTool(t *testing.T, directory, name, body string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func revisionForTag(tag string) string {
	if tag == "v1.0.0" {
		return testV1Revision
	}
	return testV2Revision
}

const fakeGitHubCLI = `#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "api" ]] || { echo "unexpected gh command" >&2; exit 2; }
printf '%s' "${FAKE_RELEASE_TAGS}"
if [[ "${FAKE_GH_MODE}" == "partial-failure" ]]; then
  echo "simulated pagination failure" >&2
  exit 1
fi
`

const fakeGoCompiler = `#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "build" ]] || { echo "unexpected fake Go command" >&2; exit 2; }
shift
output=''
while (( $# > 0 )); do
  if [[ "$1" == '-o' ]]; then
    output="$2"
    shift 2
    continue
  fi
  shift
done
[[ -n "${output}" ]] || { echo "fake Go build received no -o path" >&2; exit 2; }
mkdir -p "$(dirname "${output}")"
printf 'deterministic mysql-mcp test binary\n' > "${output}"
chmod 0755 "${output}"
`

const fakeGitCLI = `#!/usr/bin/env bash
set -euo pipefail
ref="${@: -1}"
tag="${ref%\^\{commit\}}"
case "${tag}" in
  v1.0.0) printf '%s\n' '` + testV1Revision + `' ;;
  v2.0.0) printf '%s\n' '` + testV2Revision + `' ;;
  *) echo "unknown fake Git tag: ${tag}" >&2; exit 1 ;;
esac
`

const fakeDockerCLI = `#!/usr/bin/env bash
set -euo pipefail
state_dir="${FAKE_STATE_DIR}"
operation="${3:-}"
last_arg="${@: -1}"
v1_digest='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
v2_digest='sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
tampered_digest='sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'

emit_manifest() {
  local version="$1"
  local digest="$2"
  local revision="$3"
  local platform_mode="${4:-valid}"
  local armv7='[{"digest":"sha256:0707070707070707070707070707070707070707070707070707070707070707","platform":{"os":"linux","architecture":"arm","variant":"v7"}}]'
  if [[ "${platform_mode}" == "missing-platform" ]]; then
    armv7='[]'
  fi
  jq -cn \
    --arg version "${version}" \
    --arg digest "${digest}" \
    --arg revision "${revision}" \
    --argjson armv7 "${armv7}" \
    '{
      digest: $digest,
      annotations: {
        "org.opencontainers.image.version": $version,
        "org.opencontainers.image.revision": $revision
      },
      manifests: ([
        {digest:"sha256:0303030303030303030303030303030303030303030303030303030303030303",platform:{os:"linux",architecture:"386"}},
        {digest:"sha256:0606060606060606060606060606060606060606060606060606060606060606",platform:{os:"linux",architecture:"amd64"}},
        {digest:"sha256:0404040404040404040404040404040404040404040404040404040404040404",platform:{os:"linux",architecture:"arm",variant:"v6"}},
        {digest:"sha256:0808080808080808080808080808080808080808080808080808080808080808",platform:{os:"linux",architecture:"arm64",variant:"v8"}}
      ] + $armv7)
    }'
}

emit_exact() {
  local tag="$1"
  local version digest revision platform_mode='valid'
  case "${tag}" in
    v1.0.0|1.0.0)
      version='v1.0.0'; digest="${v1_digest}"; revision='` + testV1Revision + `'
      [[ "${FAKE_EXACT_MODE}" != "bad-v1-version" ]] || version='v0.9.0'
      ;;
    v2.0.0|2.0.0)
      version='v2.0.0'; digest="${v2_digest}"; revision='` + testV2Revision + `'
      [[ "${FAKE_EXACT_MODE}" != "bad-v2-version" ]] || version='v1.9.0'
      [[ "${FAKE_EXACT_MODE}" != "missing-platform" ]] || platform_mode='missing-platform'
      if [[ "${FAKE_EXACT_MODE}" == "split-digest" && "${tag}" == "2.0.0" ]]; then
        digest="${tampered_digest}"
      fi
      ;;
    *) echo "unknown exact Docker tag: ${tag}" >&2; exit 1 ;;
  esac
  emit_manifest "${version}" "${digest}" "${revision}" "${platform_mode}"
}

if [[ "${operation}" == "inspect" ]]; then
  ref="${last_arg}"
  tag="${ref##*:}"
  if [[ "${tag}" != "latest" ]]; then
    emit_exact "${tag}"
    exit 0
  fi
  state="$(<"${state_dir}/latest-state")"
  if [[ "${state}" == "mixed-error" ]]; then
    count_file="${state_dir}/inspect-count"
    count=0
    [[ ! -f "${count_file}" ]] || count="$(<"${count_file}")"
    count=$((count + 1))
    printf '%s\n' "${count}" > "${count_file}"
    if (( count < 3 )); then
      echo "${ref}: not found" >&2
    else
      echo "dial tcp: simulated registry timeout" >&2
    fi
    exit 1
  fi
  if [[ "${state}" == "missing" ]]; then
    echo "${ref}: not found" >&2
    exit 1
  fi
  if [[ "${state}" == "ambiguous-not-found" ]]; then
    echo "manifest unknown" >&2
    exit 1
  fi
  if [[ "${state}" == "auth-proxy-404" ]]; then
    echo "failed to inspect ${ref}: authentication proxy returned status 404" >&2
    exit 1
  fi
  case "${state}" in
    v1.0.0) emit_manifest 'v1.0.0' "${v1_digest}" '` + testV1Revision + `' ;;
    v2.0.0) emit_manifest 'v2.0.0' "${v2_digest}" '` + testV2Revision + `' ;;
    tampered-v2) emit_manifest 'v2.0.0' "${tampered_digest}" '` + testV2Revision + `' ;;
    v3.0.0) emit_manifest 'v3.0.0' "${tampered_digest}" '3333333333333333333333333333333333333333' ;;
    *) echo "unknown fake latest state: ${state}" >&2; exit 1 ;;
  esac
  exit 0
fi

if [[ "${operation}" == "create" ]]; then
  source_ref="${last_arg}"
  case "${source_ref##*@}" in
    "${v1_digest}") new_state='v1.0.0' ;;
    "${v2_digest}") new_state='v2.0.0' ;;
    *) echo "unexpected promotion source: ${source_ref}" >&2; exit 1 ;;
  esac
  printf '%s\n' "${new_state}" > "${state_dir}/latest-state"
  count="$(<"${state_dir}/create-count")"
  printf '%s\n' "$((count + 1))" > "${state_dir}/create-count"
  exit 0
fi

echo "unexpected docker operation: ${operation}" >&2
exit 2
`
