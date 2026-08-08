#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Protocol-level integration probe for pkgcache's six roles:
#   OCI, npm, PyPI, apt+apk, Git, and files.
#
# Each pull client uses a fresh local cache twice. The test then verifies that the
# two results are byte-identical, pkgcache recorded an immediate cache hit, and the
# expected artifact exists in that role's SQLite-backed ledger API.

readonly CACHE_HOST="${CACHE_HOST:-localhost}"
readonly CACHE_HTTPS_PORT="${CACHE_HTTPS_PORT:-8443}"
readonly CACHE_APT_PORT="${CACHE_APT_PORT:-3142}"
readonly PROJECT="${PROJECT:-global}"
readonly TEST_PHASE="${TEST_PHASE:-online}"
readonly CA_CERT="${CA_CERT:-/certs/ca.crt}"
readonly FILES_TOKEN_FILE="${FILES_TOKEN_FILE:-/run/secrets/files_token}"
readonly RESULTS_DIR="${RESULTS_DIR:-/results}"

readonly OCI_IMAGE="${OCI_IMAGE:-dockerhub/library/alpine:3.20}"
readonly OCI_LEDGER_QUERY="${OCI_LEDGER_QUERY:-alpine}"
readonly PYPI_PACKAGE="${PYPI_PACKAGE:-idna==3.7}"
readonly PYPI_LEDGER_QUERY="${PYPI_LEDGER_QUERY:-idna}"
readonly NPM_PACKAGE="${NPM_PACKAGE:-is-number@7.0.0}"
readonly NPM_LEDGER_QUERY="${NPM_LEDGER_QUERY:-is-number}"
readonly APT_PACKAGE="${APT_PACKAGE:-hello}"
readonly APK_PACKAGE="${APK_PACKAGE:-busybox}"
readonly GIT_REPOSITORY="${GIT_REPOSITORY:-github.com/octocat/Hello-World.git}"
readonly GIT_LEDGER_QUERY="${GIT_LEDGER_QUERY:-Hello-World}"
readonly FILES_PATH="${FILES_PATH:-examples/six-repositories/probe.bin}"
readonly APT_SUITE="${APT_SUITE:-bookworm}"

readonly HTTPS_BASE="https://${CACHE_HOST}:${CACHE_HTTPS_PORT}"
readonly APT_PROXY_PROJECT="$(if [[ "${PROJECT}" == "global" ]]; then printf ''; else printf '%s@' "${PROJECT}"; fi)"
readonly APT_PROXY="http://${APT_PROXY_PROJECT}${CACHE_HOST}:${CACHE_APT_PORT}"
readonly OCI_PROJECT_PREFIX="$(if [[ "${PROJECT}" == "global" ]]; then printf ''; else printf '%s/' "${PROJECT}"; fi)"

PASS_COUNT=0
WORK_DIR=""

log() {
    printf '\n==> %s\n' "$*"
}

pass() {
    PASS_COUNT=$((PASS_COUNT + 1))
    printf '    PASS  %s\n' "$*"
}

fail() {
    printf '    FAIL  %s\n' "$*" >&2
    exit 1
}

cleanup() {
    if [[ -n "${WORK_DIR}" && -d "${WORK_DIR}" ]]; then
        rm -rf -- "${WORK_DIR}"
    fi
}
trap cleanup EXIT

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is missing: $1"
}

validate_inputs() {
    case "${TEST_PHASE}" in
        online|offline) ;;
        *) fail "TEST_PHASE must be 'online' or 'offline', got '${TEST_PHASE}'" ;;
    esac
    [[ "${PROJECT}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] \
        || fail "PROJECT contains URL-unsafe characters: ${PROJECT}"
    [[ "${FILES_PATH}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._/-]*$ ]] \
        || fail "FILES_PATH must be a relative URL path without spaces or query parameters"
    [[ -r "${CA_CERT}" ]] \
        || fail "CA certificate is not readable at ${CA_CERT}; mount certs/ca.crt there"
    [[ -d /opt/alpine && -x /opt/alpine/sbin/apk ]] \
        || fail "the embedded Alpine root filesystem is missing"

    for cmd in apt-get chroot cmp curl date dd find git jq npm pip python skopeo sha256sum sort xargs; do
        require_command "${cmd}"
    done
}

https_json() {
    curl --silent --show-error --fail \
        --connect-timeout 10 --max-time 60 \
        --cacert "${CA_CERT}" "$@"
}

expect_status() {
    local expected="$1"
    shift
    local body="${WORK_DIR}/http-body"
    local actual
    actual="$(curl --silent --show-error \
        --connect-timeout 10 --max-time 60 \
        --cacert "${CA_CERT}" \
        --output "${body}" --write-out '%{http_code}' "$@")"
    if [[ "${actual}" != "${expected}" ]]; then
        printf 'unexpected response body:\n' >&2
        sed -n '1,20p' "${body}" >&2
        fail "expected HTTP ${expected}, received ${actual}"
    fi
}

progress_url() {
    case "$1" in
        oci) printf '%s/%s/oci/v2/_progress' "${HTTPS_BASE}" "${PROJECT}" ;;
        npm) printf '%s/%s/npm/-/progress' "${HTTPS_BASE}" "${PROJECT}" ;;
        pypi) printf '%s/%s/pypi/+progress' "${HTTPS_BASE}" "${PROJECT}" ;;
        apt) printf '%s/%s/apt/acng-progress' "${HTTPS_BASE}" "${PROJECT}" ;;
        git) printf '%s/%s/git/+progress' "${HTTPS_BASE}" "${PROJECT}" ;;
        files) printf '%s/%s/files/+progress' "${HTTPS_BASE}" "${PROJECT}" ;;
        *) fail "unknown progress role: $1" ;;
    esac
}

assert_recent_hit() {
    local role="$1"
    local since="$2"
    local url
    url="$(progress_url "${role}")"

    # Recording is synchronous, but a short poll makes this robust to a streamed
    # response finishing a fraction after its client process exits.
    local attempt
    for attempt in {1..20}; do
        if https_json "${url}" \
            | jq --exit-status --argjson since "${since}" \
                'any(.recent[]?; .hit == true and .failed != true and .time >= $since)' \
                >/dev/null; then
            pass "${role}: second clean-client fetch was served from cache"
            return
        fi
        sleep 0.25
    done
    fail "${role}: no cache hit appeared in the recent-request feed"
}

assert_ledger_artifact() {
    local role="$1"
    local ecosystem="$2"
    local query="$3"
    local response="${WORK_DIR}/ledger-${role}-${ecosystem}.json"

    https_json --get \
        --data-urlencode "eco=${ecosystem}" \
        --data-urlencode "q=${query}" \
        "${HTTPS_BASE}/${PROJECT}/${role}/+ledger/artifacts" > "${response}"
    jq --exit-status '.artifacts | length > 0' "${response}" >/dev/null \
        || fail "${role}: ledger has no ${ecosystem} artifact matching '${query}'"
    pass "${role}: artifact is present in the ledger"
}

tree_manifest() {
    local directory="$1"
    (
        cd "${directory}"
        find . -type f -print0 \
            | sort -z \
            | xargs -0 -r sha256sum
    )
}

assert_same_tree() {
    local label="$1"
    local first="$2"
    local second="$3"
    local first_manifest="${WORK_DIR}/${label}-first.sha256"
    local second_manifest="${WORK_DIR}/${label}-second.sha256"

    tree_manifest "${first}" > "${first_manifest}"
    tree_manifest "${second}" > "${second_manifest}"
    [[ -s "${first_manifest}" ]] || fail "${label}: first fetch produced no files"
    cmp --silent "${first_manifest}" "${second_manifest}" \
        || fail "${label}: repeated fetches did not produce identical bytes"
    pass "${label}: two isolated clients produced identical artifacts"
}

check_health() {
    log "Health and mode checks"
    https_json "${HTTPS_BASE}/healthz" \
        | jq --exit-status '.status == "ok" and .server == "unified"' >/dev/null \
        || fail "unified HTTPS endpoint is unhealthy"

    local expected_offline=false
    [[ "${TEST_PHASE}" == "offline" ]] && expected_offline=true

    local role
    for role in oci npm pypi apt git files; do
        https_json "${HTTPS_BASE}/${PROJECT}/${role}/healthz" \
            | jq --exit-status \
                --arg role "${role}" \
                --arg project "${PROJECT}" \
                --argjson offline "${expected_offline}" \
                '.status == "ok"
                 and .role == $role
                 and .project == $project
                 and .offline == $offline' >/dev/null \
            || fail "${role}: health response does not match project/mode"
    done
    pass "all six roles are healthy in ${TEST_PHASE} mode"
}

test_oci() {
    log "1/6 OCI: complete image copy, repeatability, hit feed, and ledger"
    local first="${WORK_DIR}/oci-first"
    local second="${WORK_DIR}/oci-second"
    local cert_dir="${WORK_DIR}/oci-certs"
    mkdir -p "${first}" "${second}" "${cert_dir}"
    cp "${CA_CERT}" "${cert_dir}/ca.crt"

    local arch="${OCI_ARCH:-}"
    if [[ -z "${arch}" ]]; then
        case "$(uname -m)" in
            x86_64) arch=amd64 ;;
            aarch64) arch=arm64 ;;
            *) fail "set OCI_ARCH for unsupported machine architecture $(uname -m)" ;;
        esac
    fi
    local ref="docker://${CACHE_HOST}:${CACHE_HTTPS_PORT}/${OCI_PROJECT_PREFIX}${OCI_IMAGE}"
    local since
    since="$(date +%s.%3N)"

    skopeo copy \
        --src-tls-verify=true \
        --src-cert-dir "${cert_dir}" \
        --override-os linux \
        --override-arch "${arch}" \
        "${ref}" "dir:${first}" >/dev/null
    skopeo copy \
        --src-tls-verify=true \
        --src-cert-dir "${cert_dir}" \
        --override-os linux \
        --override-arch "${arch}" \
        "${ref}" "dir:${second}" >/dev/null

    assert_same_tree oci "${first}" "${second}"
    assert_recent_hit oci "${since}"
    assert_ledger_artifact oci docker "${OCI_LEDGER_QUERY}"
}

test_pypi() {
    log "2/6 PyPI: simple-index resolution, wheel/sdist bytes, hit feed, and ledger"
    local first="${WORK_DIR}/pypi-first"
    local second="${WORK_DIR}/pypi-second"
    mkdir -p "${first}" "${second}"
    local index="${HTTPS_BASE}/${PROJECT}/pypi/root/pypi/+simple/"
    local since
    since="$(date +%s.%3N)"

    python -m pip download \
        --quiet --disable-pip-version-check --no-cache-dir --no-deps \
        --cert "${CA_CERT}" --index-url "${index}" \
        --dest "${first}" "${PYPI_PACKAGE}"
    python -m pip download \
        --quiet --disable-pip-version-check --no-cache-dir --no-deps \
        --cert "${CA_CERT}" --index-url "${index}" \
        --dest "${second}" "${PYPI_PACKAGE}"

    assert_same_tree pypi "${first}" "${second}"
    assert_recent_hit pypi "${since}"
    assert_ledger_artifact pypi pip "${PYPI_LEDGER_QUERY}"
}

test_npm() {
    log "3/6 npm: packument resolution, tarball bytes, hit feed, and ledger"
    local first="${WORK_DIR}/npm-first"
    local second="${WORK_DIR}/npm-second"
    mkdir -p "${first}" "${second}"
    local registry="${HTTPS_BASE}/${PROJECT}/npm/"
    local since
    since="$(date +%s.%3N)"

    npm pack "${NPM_PACKAGE}" \
        --silent \
        --registry="${registry}" \
        --cafile="${CA_CERT}" \
        --cache="${WORK_DIR}/npm-cache-first" \
        --pack-destination="${first}" >/dev/null
    npm pack "${NPM_PACKAGE}" \
        --silent \
        --registry="${registry}" \
        --cafile="${CA_CERT}" \
        --cache="${WORK_DIR}/npm-cache-second" \
        --pack-destination="${second}" >/dev/null

    assert_same_tree npm "${first}" "${second}"
    assert_recent_hit npm "${since}"
    assert_ledger_artifact npm npm "${NPM_LEDGER_QUERY}"
}

apt_options() {
    local output_root="$1"
    local sources="$2"
    # The runner's temporary workspace is deliberately mode 0700, so apt's usual
    # `_apt` drop cannot traverse it. This acquisition workspace is disposable and
    # contains no host path; keep that process as root inside the runner.
    printf '%s\0' \
        -o "Acquire::http::Proxy=${APT_PROXY}" \
        -o "Acquire::https::Proxy=DIRECT" \
        -o "APT::Sandbox::User=root" \
        -o "Dir::Etc::sourcelist=${sources}" \
        -o "Dir::Etc::sourceparts=-" \
        -o "Dir::State::lists=${output_root}/lists" \
        -o "Dir::Cache=${output_root}/cache"
}

apt_fetch() {
    local output_root="$1"
    local sources="${output_root}/sources.list"
    mkdir -p \
        "${output_root}/artifact" \
        "${output_root}/lists/partial" \
        "${output_root}/cache/archives/partial"
    printf 'deb http://deb.debian.org/debian %s main\n' "${APT_SUITE}" > "${sources}"

    local -a options=()
    mapfile -d '' -t options < <(apt_options "${output_root}" "${sources}")
    apt-get "${options[@]}" update >/dev/null
    (
        cd "${output_root}/artifact"
        apt-get "${options[@]}" download "${APT_PACKAGE}" >/dev/null
    )
}

prepare_alpine_root() {
    # Docker injects these files at container runtime, so the copied rootfs needs
    # the runner's current values before apk can resolve the cache hostname.
    cp --remove-destination /etc/resolv.conf /opt/alpine/etc/resolv.conf
    cp --remove-destination /etc/hosts /opt/alpine/etc/hosts
    sed -i 's|https://|http://|g' /opt/alpine/etc/apk/repositories
}

apk_fetch() {
    local output_name="$1"
    local output_host="/opt/alpine/tmp/${output_name}"
    mkdir -p "${output_host}"
    http_proxy="${APT_PROXY}" \
        chroot /opt/alpine /sbin/apk fetch \
            --no-cache \
            --output "/tmp/${output_name}" \
            "${APK_PACKAGE}" >/dev/null
}

test_apt_apk() {
    log "4/6 apt+apk: two real clients, clean indexes, package bytes, hit feed, and ledger"
    local apt_first="${WORK_DIR}/apt-first"
    local apt_second="${WORK_DIR}/apt-second"
    local since
    since="$(date +%s.%3N)"

    apt_fetch "${apt_first}"
    apt_fetch "${apt_second}"
    assert_same_tree apt "${apt_first}/artifact" "${apt_second}/artifact"

    prepare_alpine_root
    apk_fetch pkgcache-apk-first
    apk_fetch pkgcache-apk-second
    assert_same_tree apk \
        /opt/alpine/tmp/pkgcache-apk-first \
        /opt/alpine/tmp/pkgcache-apk-second

    assert_recent_hit apt "${since}"
    assert_ledger_artifact apt apt "${APT_PACKAGE}"
    assert_ledger_artifact apt apk "${APK_PACKAGE}"
}

git_clone() {
    local destination="$1"
    GIT_TERMINAL_PROMPT=0 \
    GIT_SSL_CAINFO="${CA_CERT}" \
        git -c protocol.version=2 clone \
            --quiet --depth 1 --no-tags \
            "${HTTPS_BASE}/${PROJECT}/git/${GIT_REPOSITORY}" \
            "${destination}"
}

test_git() {
    log "5/6 Git: smart-HTTP clone twice, object verification, push refusal, hit feed, and ledger"
    local first="${WORK_DIR}/git-first"
    local second="${WORK_DIR}/git-second"
    local since
    since="$(date +%s.%3N)"

    git_clone "${first}"
    git_clone "${second}"
    git -C "${first}" fsck --full --no-dangling >/dev/null
    git -C "${second}" fsck --full --no-dangling >/dev/null

    local first_head
    local second_head
    first_head="$(git -C "${first}" rev-parse HEAD)"
    second_head="$(git -C "${second}" rev-parse HEAD)"
    [[ "${first_head}" == "${second_head}" ]] \
        || fail "git: repeated clones resolved to different commits"
    pass "git: two verified clones resolved to ${first_head:0:12}"

    expect_status 403 \
        "${HTTPS_BASE}/${PROJECT}/git/${GIT_REPOSITORY}/info/refs?service=git-receive-pack"
    pass "git: the mirror explicitly refuses push negotiation"

    assert_recent_hit git "${since}"
    assert_ledger_artifact git git "${GIT_LEDGER_QUERY}"
}

load_files_token() {
    local token="${FILES_TOKEN:-}"
    if [[ -z "${token}" && -r "${FILES_TOKEN_FILE}" ]]; then
        IFS= read -r token < "${FILES_TOKEN_FILE}" || true
    fi
    printf '%s' "${token}"
}

make_files_payload() {
    local payload="$1"
    {
        printf 'pkgcache six-repository integration fixture\n'
        printf 'project=%s\n' "${PROJECT}"
        dd if=/dev/zero bs=1024 count=64 status=none
        printf '\nend-of-fixture\n'
    } > "${payload}"
}

test_files() {
    log "6/6 files: auth failures, checksum upload, write-once, HEAD, Range, repeat GET, and ledger"
    local payload="${WORK_DIR}/files-payload"
    local first="${WORK_DIR}/files-first"
    local second="${WORK_DIR}/files-second"
    local range="${WORK_DIR}/files-range"
    local expected_range="${WORK_DIR}/files-range-expected"
    local upload_response="${WORK_DIR}/files-upload.json"
    local auth_headers="${WORK_DIR}/files-auth.headers"
    local url="${HTTPS_BASE}/${PROJECT}/files/${FILES_PATH}"
    local since
    since="$(date +%s.%3N)"
    make_files_payload "${payload}"

    local token
    token="$(load_files_token)"
    if [[ "${TEST_PHASE}" == "online" ]]; then
        [[ -n "${token}" ]] \
            || fail "online files test needs FILES_TOKEN or a readable FILES_TOKEN_FILE"
        printf 'Authorization: Bearer %s\n' "${token}" > "${auth_headers}"
        chmod 0600 "${auth_headers}"

        expect_status 401 \
            --request PUT \
            --upload-file "${payload}" \
            --header 'Authorization: Bearer deliberately-invalid' \
            "${url}.auth-rejected"
        pass "files: an invalid write token is rejected"

        expect_status 400 \
            --request PUT \
            --upload-file "${payload}" \
            --header @"${auth_headers}" \
            --header 'X-Checksum-Sha256: deliberately-wrong' \
            "${url}.checksum-rejected"
        pass "files: a bad client checksum is rejected before commit"

        local payload_sha
        payload_sha="$(sha256sum "${payload}" | awk '{print $1}')"
        curl --silent --show-error --fail \
            --connect-timeout 10 --max-time 120 \
            --cacert "${CA_CERT}" \
            --request PUT \
            --upload-file "${payload}" \
            --header @"${auth_headers}" \
            --header "X-Checksum-Sha256: ${payload_sha}" \
            "${url}?overwrite=1" > "${upload_response}"
        jq --exit-status --arg sha "${payload_sha}" \
            '.sha256 == $sha and .size > 65536' "${upload_response}" >/dev/null \
            || fail "files: upload response did not confirm checksum and size"
        pass "files: token-gated upload committed the expected checksum"

        expect_status 409 \
            --request PUT \
            --upload-file "${payload}" \
            --header @"${auth_headers}" \
            "${url}"
        pass "files: write-once protection rejects an accidental replacement"
    else
        if [[ -n "${token}" ]]; then
            printf 'Authorization: Bearer %s\n' "${token}" > "${auth_headers}"
            chmod 0600 "${auth_headers}"
            expect_status 403 \
                --request PUT \
                --upload-file "${payload}" \
                --header @"${auth_headers}" \
                "${url}?overwrite=1"
            pass "files: offline mode rejects an otherwise-authorized write"
        fi
    fi

    local head_status
    head_status="$(curl --silent --show-error \
        --connect-timeout 10 --max-time 60 \
        --cacert "${CA_CERT}" \
        --head --output /dev/null --write-out '%{http_code}' "${url}")"
    [[ "${head_status}" == "200" ]] || fail "files: HEAD returned HTTP ${head_status}"
    pass "files: HEAD succeeds without downloading the body"

    local range_status
    range_status="$(curl --silent --show-error \
        --connect-timeout 10 --max-time 60 \
        --cacert "${CA_CERT}" \
        --range 128-4095 \
        --output "${range}" --write-out '%{http_code}' "${url}")"
    [[ "${range_status}" == "206" ]] || fail "files: Range returned HTTP ${range_status}, expected 206"
    dd if="${payload}" of="${expected_range}" bs=1 skip=128 count=3968 status=none
    cmp --silent "${range}" "${expected_range}" \
        || fail "files: Range response bytes do not match the uploaded fixture"
    pass "files: byte-range response is exact"

    curl --silent --show-error --fail \
        --connect-timeout 10 --max-time 120 \
        --cacert "${CA_CERT}" --output "${first}" "${url}"
    curl --silent --show-error --fail \
        --connect-timeout 10 --max-time 120 \
        --cacert "${CA_CERT}" --output "${second}" "${url}"
    cmp --silent "${payload}" "${first}" \
        || fail "files: downloaded bytes differ from the fixture"
    cmp --silent "${first}" "${second}" \
        || fail "files: repeated downloads differ"
    pass "files: repeated full downloads are byte-identical"

    assert_recent_hit files "${since}"
    assert_ledger_artifact files files "${FILES_PATH##*/}"
}

write_summary() {
    mkdir -p "${RESULTS_DIR}"
    jq --null-input \
        --arg status "passed" \
        --arg phase "${TEST_PHASE}" \
        --arg project "${PROJECT}" \
        --arg cache "${CACHE_HOST}:${CACHE_HTTPS_PORT}" \
        --argjson checks "${PASS_COUNT}" \
        '{
            status: $status,
            phase: $phase,
            project: $project,
            cache: $cache,
            checks_passed: $checks
        }' > "${RESULTS_DIR}/summary.json"
}

main() {
    validate_inputs
    WORK_DIR="$(mktemp -d /tmp/pkgcache-six-repositories.XXXXXX)"

    printf 'pkgcache six-role integration test\n'
    printf '  cache:   %s:%s (apt/apk :%s)\n' \
        "${CACHE_HOST}" "${CACHE_HTTPS_PORT}" "${CACHE_APT_PORT}"
    printf '  project: %s\n' "${PROJECT}"
    printf '  phase:   %s\n' "${TEST_PHASE}"

    check_health
    test_oci
    test_pypi
    test_npm
    test_apt_apk
    test_git
    test_files
    write_summary

    log "All six repository roles passed (${PASS_COUNT} assertions)"
    printf 'Machine-readable summary: %s/summary.json\n' "${RESULTS_DIR}"
}

main "$@"
