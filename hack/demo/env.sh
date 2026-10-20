#!/usr/bin/env bash
# Shared local configuration helpers. Never print configuration values.
#
# demo_env_load is the only way the demo scripts read the demo environment
# file (its argument, else SPROOZI_DEMO_ENV_FILE, default .local/demo.env). It checks the file
# with demo_env_check before bash `source` sees it.

# Shell and tool variables that make bash, or a program the demo runs, execute
# code or look up commands somewhere else. The demo never needs them in its
# file, and child bash processes would act on them.
demo_env_denied='^(BASH_ENV|ENV|PROMPT_COMMAND|PS[0-4]|SHELLOPTS|BASHOPTS|IFS|PATH|CDPATH|GLOBIGNORE|HOME|SHELL|INPUTRC|HISTFILE|BASH_.*|LD_.*|DYLD_.*|PYTHON.*|PERL5LIB|PERL5OPT|PERLLIB|RUBYOPT|RUBYLIB|NODE_OPTIONS|GIT_.*|GOFLAGS|GOENV|GOTOOLCHAIN|GOROOT|MAKEFLAGS|MFLAGS|MAKEFILES|MAKELEVEL|SSH_ASKPASS|EDITOR|VISUAL|PAGER|GH_PAGER|BROWSER|OPENSSL_CONF|OPENSSL_ENGINES)$'

# Check that every line of the demo environment file is a plain assignment to
# an allowed name. A line such as `KEY=sk x` would otherwise run `x` as a
# command when the file is sourced. Values may use the quoting that
# configure.sh writes (printf %q) and $NAME or ${NAME} references; command
# substitution, unquoted spaces and multi-line values are rejected. Errors name
# the line and, for a refused name, the name, never a value.
demo_env_check() {
  local file="$1"
  local name='[A-Za-z_][A-Za-z0-9_]*' plain sq dq ansi var value line number=0 key
  var="[\$]($name|[{]$name[}])"
  plain="[^][:space:]\"'\\\$\`;&|<>()]"
  sq="'[^']*'"
  dq="\"([^\"\\\$\`]|\\\\.|$var)*\""
  ansi="[\$]'([^'\\\\]|\\\\.)*'"
  value="($plain|$sq|$dq|$ansi|\\\\.|$var)*"
  local assignment="^[[:space:]]*(export[[:space:]]+)?($name)=$value([[:space:]]+(#.*)?)?$"
  local blank='^[[:space:]]*(#.*)?$' exported="^[[:space:]]*export(([[:space:]]+$name)+)[[:space:]]*$"
  if [[ ! -f "$file" || ! -r "$file" ]]; then
    echo "Cannot read $file" >&2
    return 1
  fi
  if LC_ALL=C grep -q $'\r' "$file"; then
    echo "$file has Windows (CRLF) line endings; save it with LF line endings, for example: perl -pi -e 's/\\r\$//' $file" >&2
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    number=$((number + 1))
    [[ "$line" =~ $blank ]] && continue
    if [[ "$line" =~ $assignment ]]; then
      key="${BASH_REMATCH[2]}"
    elif [[ "$line" =~ $exported ]]; then
      key=''
      for key in ${BASH_REMATCH[1]}; do
        [[ ! "$key" =~ $demo_env_denied ]] || break
      done
    else
      echo "$file line $number is not a plain KEY=value assignment; the demo loads this file with bash source, so quote values that contain spaces and remove anything that would run a command" >&2
      return 1
    fi
    if [[ "$key" =~ $demo_env_denied ]]; then
      echo "$file line $number sets $key, which the demo refuses because bash or a tool it runs would act on it; remove that line" >&2
      return 1
    fi
  done <"$file"
}

# demo_env_load [FILE] reads FILE, or SPROOZI_DEMO_ENV_FILE when FILE is not
# given.
demo_env_load() {
  demo_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  demo_env_file="${1:-${SPROOZI_DEMO_ENV_FILE:-$demo_repo_root/.local/demo.env}}"
  if [[ -L "$demo_env_file" ]]; then
    echo 'Refusing to read or write demo configuration through a symlink' >&2
    return 1
  fi
  if [[ -e "$demo_env_file" ]]; then
    demo_env_check "$demo_env_file" || return 1
    local caller_name caller_assignment caller_env=()
    while IFS= read -r caller_name; do
      [[ -z "${!caller_name:-}" ]] || caller_env+=("$caller_name=${!caller_name}")
    done < <(compgen -e)
    set -a
    # shellcheck disable=SC1090
    source "$demo_env_file"
    set +a
    # Non-empty values the caller exported win over the file, so
    # `make demo IMG=...` beats an IMG in the file. Readonly shell variables
    # cannot be re-exported.
    for caller_assignment in ${caller_env[@]+"${caller_env[@]}"}; do
      export "$caller_assignment" 2>/dev/null || true
    done
  fi
}

demo_env_set() {
  local key="$1" value="$2" temporary
  umask 077
  mkdir -p "$(dirname "$demo_env_file")"
  temporary="$(mktemp "${demo_env_file}.XXXXXX")"
  if [[ -f "$demo_env_file" ]]; then
    awk -v key="$key" 'index($0, key "=") != 1 { print }' "$demo_env_file" >"$temporary"
  fi
  printf '%s=%q\n' "$key" "$value" >>"$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$demo_env_file"
  printf -v "$key" '%s' "$value"
  export "$key"
}
