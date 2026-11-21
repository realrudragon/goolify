#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf -- "$test_dir"' EXIT
export GIT_AUTHOR_NAME='Upgrade Test' GIT_COMMITTER_NAME='Upgrade Test'
export GIT_AUTHOR_EMAIL='upgrade@example.test' GIT_COMMITTER_EMAIL='upgrade@example.test'
export GIT_CONFIG_NOSYSTEM=1
export GIT_TERMINAL_PROMPT=0
mkdir -p "$test_dir/bin"
cat >"$test_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DOCKER_CALLS"
exit "${DOCKER_EXIT_CODE:-0}"
EOF
chmod +x "$test_dir/bin/docker"
export PATH="$test_dir/bin:$PATH"

prepare() {
  local name="$1"
  local scenario="$test_dir/$update_command-$name"
  mkdir -p "$scenario/author/scripts"
  git init --quiet --bare "$scenario/origin"
  git -C "$scenario/author" init --quiet -b main
  cp "$repo/scripts/dev" "$scenario/author/scripts/dev"
  chmod 644 "$scenario/author/scripts/dev"
  printf 'initial\n' >"$scenario/author/version"
  git -C "$scenario/author" add .
  git -C "$scenario/author" commit --quiet -m initial
  git -C "$scenario/author" remote add origin "$scenario/origin"
  git -C "$scenario/author" push --quiet -u origin main
  git clone --quiet --branch main "$scenario/origin" "$scenario/server"
  chmod +x "$scenario/server/scripts/dev"
  printf 'ROOT_USER_EMAIL=test@example.test\n' >"$scenario/server/.env"
  chmod +x "$scenario/author/scripts/dev"
  printf 'updated\n' >"$scenario/author/version"
  git -C "$scenario/author" add .
  git -C "$scenario/author" commit --quiet -m update
  git -C "$scenario/author" push --quiet origin main
  GOOLIFY_DIR="$scenario/server"
  ENV_FILE="$GOOLIFY_DIR/.env"
  wrapper="$scenario/goolify"
  # Render only the installer's real wrapper template, without running installation.
  awk '/^cat >\/usr\/local\/bin\/goolify <<EOF$/ { copy=1 } copy { print } copy && /^EOF$/ { exit }' "$repo/scripts/install.sh" |
    sed 's|^cat >/usr/local/bin/goolify <<EOF$|cat >"$wrapper" <<EOF|' >"$scenario/render-wrapper.sh"
  test -s "$scenario/render-wrapper.sh"
  source "$scenario/render-wrapper.sh"
  chmod +x "$wrapper"
  export DOCKER_CALLS="$scenario/docker-calls"
}

for update_command in update upgrade; do
  printf 'Testing goolify %s\n' "$update_command"
  prepare mode_only
  "$wrapper"
  grep -qx 'compose -f compose.yml ps' "$DOCKER_CALLS"
  test "$("$wrapper" creds)" = 'ROOT_USER_EMAIL=test@example.test'
  help_output="$("$wrapper" help)"
  grep -q 'sudo goolify update' <<<"$help_output"
  if grep -q './scripts/dev' <<<"$help_output"; then
    printf 'FAIL: installed help should describe the public goolify commands\n' >&2
    exit 1
  fi
  : >"$DOCKER_CALLS"
  (cd "$test_dir" && "$wrapper" logs worker)
  grep -qx 'compose -f compose.yml logs -f --tail=100 worker' "$DOCKER_CALLS"
  "$wrapper" restart worker
  grep -qx 'compose -f compose.yml restart worker' "$DOCKER_CALLS"
  "$wrapper" start >"$test_dir/start.log"
  grep -q 'sudo goolify creds' "$test_dir/start.log"
  if grep -q 'CoolifyAdmin123!' "$test_dir/start.log"; then
    printf 'FAIL: start must not print a development password for installed instances\n' >&2
    exit 1
  fi
  : >"$DOCKER_CALLS"
  if ! "$wrapper" "$update_command" --no-deps worker >"$test_dir/mode-only.log" 2>&1; then
    cat "$test_dir/mode-only.log" >&2
    exit 1
  fi
  test "$(cat "$GOOLIFY_DIR/version")" = updated
  grep -qx 'compose -f compose.yml up -d --build --no-deps worker' "$DOCKER_CALLS"
  test "$(cat "$GOOLIFY_DIR/.env")" = 'ROOT_USER_EMAIL=test@example.test'
  printf 'PASS: permission-only changes permit upgrade and retain environment\n'

  prepare content_change
  printf '\n# Local customization\n' >>"$GOOLIFY_DIR/scripts/dev"
  if "$wrapper" "$update_command" >"$test_dir/content-change.log" 2>&1; then
    printf 'FAIL: upgrade succeeded despite a local content conflict\n' >&2
    exit 1
  fi
  test ! -e "$DOCKER_CALLS"
  grep -q 'Local customization' "$GOOLIFY_DIR/scripts/dev"
  test "$(cat "$GOOLIFY_DIR/version")" = initial
  printf 'PASS: content conflicts abort before container rebuild\n'

  prepare fetch_failure
  git -C "$GOOLIFY_DIR" remote set-url origin "$test_dir/nonexistent-origin"
  if "$wrapper" "$update_command" >"$test_dir/fetch-failure.log" 2>&1; then
    printf 'FAIL: upgrade succeeded despite an unreachable origin\n' >&2
    exit 1
  fi
  test ! -e "$DOCKER_CALLS"
  test "$(cat "$GOOLIFY_DIR/version")" = initial
  printf 'PASS: failed fetch aborts before container rebuild\n'

  prepare build_failure
  status=0
  DOCKER_EXIT_CODE=42 "$wrapper" "$update_command" >"$test_dir/build-failure.log" 2>&1 || status=$?
  test "$status" = 42
  printf 'PASS: failed container build propagates its exit status\n'
done
