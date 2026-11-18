#!/usr/bin/env bash
set -euo pipefail

# Deterministic check: under `set -o pipefail`, a pipeline stage that stops reading
# early makes the upstream writer die of SIGPIPE (exit 141), so the whole pipeline
# fails or becomes timing-dependent. That already caused two flaky CI runs (#2, #10,
# fixed in 2cd7e2a).
#
# GitHub Actions' default Linux shell is `bash -e {0}` (no pipefail); an explicit
# `shell: bash` becomes `bash --noprofile --norc -eo pipefail {0}`. Every run: block
# is scanned anyway: that is conservative, and a step can opt into `shell: bash`.
#
# Scanned:
#   - every tracked/untracked bash/sh script with a shebang and a `set ...pipefail` line;
#   - bash `run = '''...'''` task bodies in mise.toml that declare pipefail;
#   - `run:` blocks in .github/workflows/*.yml and composite action.yml files.
#
# Rejected pipeline stages (the command after a `|`):
#   - head;
#   - grep/rg with -q/--quiet/--silent, -m/--max-count, -l/--files-with-matches,
#     -L/--files-without-match (also in bundled short flags such as `grep -Eq`);
#   - sed with a `q`/`Q` command, awk with `exit`, and a standalone `read`.
#
# Fix: collect the output into a variable first and match there
# (`[[ $var == *pat* ]]`, `grep -q <<<"$var"`), or use a command that drains the
# input (`grep -c`). When the early exit is deliberate, mark it on the pipeline's
# logical line or on the line directly above it:
#   # pipefail-ok: <reason>

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

violations=0
whitelist_re='#[[:space:]]*pipefail-ok:[[:space:]]*[^[:space:]]'

fail() {
  printf 'pipefail pipeline check: %s\n' "$*" >&2
  exit 1
}

report() {
  local file=$1 lineno=$2 reason=$3
  printf '%s:%s: %s\n' "$file" "$lineno" "$reason" >&2
  printf '    fix: collect the output into a variable first and match there ' >&2
  printf '([[ $var == *pat* ]], grep -q <<<"$var", or grep -c), or add a '\''# pipefail-ok: <reason>'\'' comment\n' >&2
  violations=$((violations + 1))
}

# True when the line ends with an unquoted `\` (explicit continuation) or an
# unquoted `|` (bash allows a newline after a pipe operator). Trailing comments
# and quoted text are ignored.
continues() {
  local s=$1 c q= i n last=
  n=${#s}
  for ((i = 0; i < n; i++)); do
    c=${s:i:1}
    if [[ -n $q ]]; then
      [[ $c == "$q" ]] && q=
      [[ $c == [[:space:]] ]] || last=$c
      continue
    fi
    case $c in
    "'" | '"')
      q=$c
      last=$c
      ;;
    '#')
      if ((i == 0)) || [[ ${s:i-1:1} == [[:space:]] || ${s:i-1:1} == ';' ]]; then
        break
      fi
      last=$c
      ;;
    '\\')
      last='\'
      i=$((i + 1))
      ;;
    [[:space:]]) ;;
    *) last=$c ;;
    esac
  done
  [[ $last == '\' || $last == '|' ]]
}

# Print each pipeline stage of one logical line on its own line. `||` is not a pipe,
# `|&` is, and `|` inside quotes or after an unquoted `#` is ignored. A `$()` inside
# double quotes is entered as code, so `x="$(git tag | head)"` is split as well.
pipeline_stages() {
  local s=$1 out= c i n state=code
  local -a sub_depth=()
  n=${#s}
  for ((i = 0; i < n; i++)); do
    c=${s:i:1}
    if [[ $state == single ]]; then
      out+=$c
      [[ $c == "'" ]] && state=code
      continue
    fi
    if [[ $state == double ]]; then
      case $c in
      '"')
        state=code
        out+=$c
        ;;
      '\\')
        out+=$c
        if ((i + 1 < n)); then
          out+=${s:i+1:1}
          i=$((i + 1))
        fi
        ;;
      '$')
        if ((i + 1 < n)) && [[ ${s:i+1:1} == '(' ]]; then
          out+='$('
          i=$((i + 1))
          sub_depth+=(0)
          state=code
        else
          out+=$c
        fi
        ;;
      *) out+=$c ;;
      esac
      continue
    fi
    case $c in
    "'")
      state=single
      out+=$c
      ;;
    '"')
      state=double
      out+=$c
      ;;
    '#')
      if ((i == 0)) || [[ ${s:i-1:1} == [[:space:]] || ${s:i-1:1} == ';' || ${s:i-1:1} == '|' ]]; then
        break
      fi
      out+=$c
      ;;
    '\\')
      out+=$c
      if ((i + 1 < n)); then
        out+=${s:i+1:1}
        i=$((i + 1))
      fi
      ;;
    '$')
      if ((i + 1 < n)) && [[ ${s:i+1:1} == '(' ]]; then
        if ((${#sub_depth[@]} > 0)); then
          sub_depth[-1]=$((sub_depth[-1] + 1))
        fi
        out+='$('
        i=$((i + 1))
      else
        out+=$c
      fi
      ;;
    '(')
      if ((${#sub_depth[@]} > 0)); then
        sub_depth[-1]=$((sub_depth[-1] + 1))
      fi
      out+=$c
      ;;
    ')')
      if ((${#sub_depth[@]} > 0)); then
        if ((sub_depth[-1] > 0)); then
          sub_depth[-1]=$((sub_depth[-1] - 1))
        else
          unset 'sub_depth[-1]'
          state=double
        fi
      fi
      out+=$c
      ;;
    '|')
      if ((i + 1 < n)) && [[ ${s:i+1:1} == '|' ]]; then
        out+='||'
        i=$((i + 1))
        continue
      fi
      if ((i + 1 < n)) && [[ ${s:i+1:1} == '&' ]]; then
        i=$((i + 1))
      fi
      printf '%s\n' "$out"
      out=
      ;;
    *) out+=$c ;;
    esac
  done
  printf '%s\n' "$out"
}

# Print the delimiters of heredocs opened on one logical line, ignoring quoted text
# and comments. `<<EOF`, `<<'EOF'`, `<<"EOF"`, `<<-EOF` and `<<\EOF` are recognised;
# `<<<` (here-string) is not a heredoc.
heredoc_delimiters() {
  local s=$1 c q= q2= i n j d strip_tabs=0
  n=${#s}
  i=0
  while ((i < n)); do
    c=${s:i:1}
    if [[ -n $q ]]; then
      [[ $c == "$q" ]] && q=
      i=$((i + 1))
      continue
    fi
    case $c in
    "'" | '"')
      q=$c
      i=$((i + 1))
      ;;
    '\\')
      i=$((i + 2))
      ;;
    '#')
      if ((i == 0)) || [[ ${s:i-1:1} == [[:space:]] || ${s:i-1:1} == ';' ]]; then
        break
      fi
      i=$((i + 1))
      ;;
    '<')
      if ((i + 1 < n)) && [[ ${s:i+1:1} == '<' ]]; then
        j=$((i + 2))
        if ((j < n)) && [[ ${s:j:1} == '<' ]]; then
          i=$((j + 1))
          continue
        fi
        strip_tabs=0
        if ((j < n)) && [[ ${s:j:1} == '-' ]]; then
          j=$((j + 1))
          strip_tabs=1
        fi
        while ((j < n)) && [[ ${s:j:1} == [[:space:]] ]]; do
          j=$((j + 1))
        done
        d=
        if ((j < n)) && [[ ${s:j:1} == "'" || ${s:j:1} == '"' ]]; then
          q2=${s:j:1}
          j=$((j + 1))
          while ((j < n)) && [[ ${s:j:1} != "$q2" ]]; do
            d+=${s:j:1}
            j=$((j + 1))
          done
          ((j < n)) && j=$((j + 1))
        else
          while ((j < n)); do
            c=${s:j:1}
            [[ $c == [[:space:]] || $c == ';' || $c == '|' || $c == '&' ]] && break
            d+=$c
            j=$((j + 1))
          done
          d=${d#\\}
        fi
        [[ -n $d ]] && printf '%s\t%s\n' "$strip_tabs" "$d"
        i=$j
        continue
      fi
      i=$((i + 1))
      ;;
    *) i=$((i + 1)) ;;
    esac
  done
}

check_stage() {
  local file=$1 lineno=$2 stage=$3
  local trimmed cmd tok i=0 j flags reason= script= line=
  trimmed=${stage#"${stage%%[![:space:]]*}"}
  [[ -n $trimmed ]] || return 0
  local -a toks=()
  read -r -a toks <<<"$trimmed"
  local wrapper= value_opts=
  while ((i < ${#toks[@]})); do
    tok=${toks[i]}
    case $tok in
    [A-Za-z_][A-Za-z0-9_]*=*)
      i=$((i + 1))
      ;;
    # Wrappers: skip their options and the positional arguments they consume before
    # the real command (e.g. `timeout 5 head`, `nice -n 5 head`, `xargs -n 1 head`).
    env | timeout | stdbuf | xargs | nice | sudo | time | nohup)
      wrapper=$tok
      value_opts=
      case $wrapper in
      env) value_opts='-u --unset -C --chdir -S --split-string' ;;
      timeout) value_opts='-s --signal -k --kill-after' ;;
      stdbuf) value_opts='-i -o -e --input --output --error' ;;
      xargs)
        value_opts='-n --max-args -I --replace -L --max-lines -P --max-procs -s --max-chars -d --delimiter -E --eof -a --arg-file --process-slot-var'
        ;;
      nice) value_opts='-n --adjustment' ;;
      sudo)
        value_opts='-u --user -g --group -p --prompt -C --close-from -h --host -r --role -t --type -U --other-user -T --command-timeout'
        ;;
      esac
      i=$((i + 1))
      while ((i < ${#toks[@]})); do
        tok=${toks[i]}
        case $tok in
        --)
          i=$((i + 1))
          break
          ;;
        -?*)
          if [[ " $value_opts " == *" $tok "* ]]; then
            i=$((i + 2))
          else
            i=$((i + 1))
          fi
          ;;
        *) break ;;
        esac
      done
      case $wrapper in
      timeout)
        # DURATION is the first positional argument.
        ((i < ${#toks[@]})) && i=$((i + 1))
        ;;
      nice)
        # The obsolete `nice -5 cmd` form is already an option; `nice 5 cmd` is not
        # valid, but accept a bare adjustment defensively.
        if ((i < ${#toks[@]})) && [[ ${toks[i]} =~ ^[+-]?[0-9]+$ ]]; then
          i=$((i + 1))
        fi
        ;;
      esac
      ;;
    command | exec | builtin)
      # `command -v head` is a lookup, not an execution: leave the option as the
      # candidate command so it does not get mistaken for `head`.
      i=$((i + 1))
      ;;
    *) break ;;
    esac
  done
  ((i < ${#toks[@]})) || return 0
  cmd=${toks[i]}
  cmd=${cmd##*/}
  while [[ $cmd == '('* || $cmd == '{'* ]]; do
    cmd=${cmd#?}
  done
  while [[ $cmd == *';' || $cmd == *')' || $cmd == *'}' || $cmd == *'&' ]]; do
    cmd=${cmd%?}
  done
  case $cmd in
  head)
    reason='stage uses head, which stops reading after the requested lines'
    ;;
  grep | egrep | fgrep | rg | ripgrep)
    for ((j = i + 1; j < ${#toks[@]}; j++)); do
      tok=${toks[j]}
      case $tok in
      --) break ;;
      --quiet | --silent)
        reason='pattern search exits at the first match'
        break
        ;;
      --max-count | --max-count=*)
        reason='pattern search stops after --max-count matches'
        break
        ;;
      --files-with-matches | --files-without-match)
        reason='pattern search stops at the first match'
        break
        ;;
      --*) ;;
      -?*)
        flags=${tok#-}
        case $flags in
        *q*)
          reason='-q exits at the first match'
          break
          ;;
        *m*)
          reason='-m stops after the requested matches'
          break
          ;;
        *l* | *L*)
          reason='-l/-L stops at the first match'
          break
          ;;
        esac
        ;;
      *) ;;
      esac
    done
    ;;
  sed)
    local sed_q_re='(^|[;{}()[:space:]])([0-9]+|\$|/[^/]*/)?\{?[qQ]([;})]|$)'
    for ((j = i + 1; j < ${#toks[@]}; j++)); do
      tok=${toks[j]}
      case $tok in
      --) break ;;
      -e | --expression)
        j=$((j + 1))
        ((j < ${#toks[@]})) || break
        tok=${toks[j]}
        script+=${tok//[\'\"]/}$'\n'
        ;;
      -f | --file)
        j=$((j + 1))
        ;;
      -?*) ;;
      *) script+=${tok//[\'\"]/}$'\n' ;;
      esac
    done
    while IFS= read -r line; do
      if [[ $line =~ $sed_q_re ]]; then
        reason='sed q/Q stops reading the rest of the input'
        break
      fi
    done <<<"$script"
    ;;
  awk | gawk | mawk)
    # `read -a` splits the quoted program into several tokens, so scan them all.
    # Match `exit` as a word, not `exit_code`; skip -v/-F values, which are not code.
    for ((j = i + 1; j < ${#toks[@]}; j++)); do
      tok=${toks[j]}
      case $tok in
      --) break ;;
      -v | --assign | -F | --field-separator)
        j=$((j + 1))
        ;;
      -f | --file) break ;;
      -?*) ;;
      *)
        if [[ $tok =~ (^|[^A-Za-z0-9_])exit([^A-Za-z0-9_]|$) ]]; then
          reason='awk exit stops reading the rest of the input'
          break
        fi
        ;;
      esac
    done
    ;;
  read)
    reason='a standalone read consumes one line and leaves the writer with SIGPIPE'
    ;;
  esac
  [[ -n $reason ]] && report "$file" "$lineno" "$reason"
  return 0
}

# scan_stream FILE [NUMBERED]
# With NUMBERED=1 each input line is "lineno<TAB>content" (used for extracted
# mise/workflow blocks); otherwise the stream's own line count is used.
scan_stream() {
  local file=$1 numbered=${2:-0}
  local lineno=0 start=0 line logical= group_whitelisted=false prev_whitelisted=false exempt
  local entry strip_tabs delimiter candidate
  local -a stages=() heredocs=() new_heredocs=()
  while IFS= read -r line || [[ -n $line ]]; do
    if ((numbered)); then
      lineno=${line%%$'\t'*}
      line=${line#*$'\t'}
    else
      lineno=$((lineno + 1))
    fi
    # Skip heredoc bodies: their content is data, not shell code.
    if ((${#heredocs[@]} > 0)); then
      entry=${heredocs[0]}
      strip_tabs=${entry%%$'\t'*}
      delimiter=${entry#*$'\t'}
      candidate=$line
      if [[ $strip_tabs == 1 ]]; then
        while [[ $candidate == $'\t'* ]]; do
          candidate=${candidate#$'\t'}
        done
      fi
      if [[ $candidate == "$delimiter" ]]; then
        heredocs=("${heredocs[@]:1}")
      fi
      continue
    fi
    if [[ -z $logical ]]; then
      logical=$line
      start=$lineno
    else
      logical+=" $line"
    fi
    [[ $line =~ $whitelist_re ]] && group_whitelisted=true
    if continues "$logical"; then
      [[ $logical == *\\ ]] && logical=${logical%\\}
      continue
    fi
    exempt=false
    [[ $group_whitelisted == true || $prev_whitelisted == true ]] && exempt=true
    if [[ $exempt == false ]]; then
      mapfile -t stages < <(pipeline_stages "$logical")
      for ((i = 1; i < ${#stages[@]}; i++)); do
        check_stage "$file" "$start" "${stages[i]}"
      done
    fi
    mapfile -t new_heredocs < <(heredoc_delimiters "$logical")
    if ((${#new_heredocs[@]} > 0)); then
      heredocs+=("${new_heredocs[@]}")
    fi
    prev_whitelisted=$group_whitelisted
    group_whitelisted=false
    logical=
  done
}

# Bash task bodies in mise.toml: emit "lineno<TAB>content" only for blocks that
# declare pipefail, so comments such as the 2cd7e2a note are still stripped later.
extract_mise_blocks() {
  awk '
    $0 == "run = " sq { inblock = 1; buf = ""; next }
    inblock && $0 == sq {
      if (buf ~ /set[[:space:]]+[^#]*pipefail/) printf "%s", buf
      inblock = 0
      next
    }
    inblock { buf = buf NR "\t" $0 "\n" }
  ' sq="'''" "$1"
}

# Emit "lineno<TAB>content" for every run: value, including block scalars.
extract_run_blocks() {
  awk '
    function indent(s) { return match(s, /^ */) ? RLENGTH : 0 }
    inblock {
      if ($0 ~ /^[[:space:]]*$/ || indent($0) > base) {
        printf "%d\t%s\n", NR, $0
        next
      }
      inblock = 0
    }
    match($0, /^[[:space:]]*(-[[:space:]]+)?run:[[:space:]]*/) {
      rest = substr($0, RSTART + RLENGTH)
      if (rest ~ /^[|>][-+]?[[:space:]]*$/) {
        inblock = 1
        base = indent($0)
        next
      }
      if (rest != "") printf "%d\t%s\n", NR, rest
    }
  ' "$1"
}

scan_workflow_file() {
  scan_stream "$1" 1 < <(extract_run_blocks "$1")
}

# Self-test: every rule must fire on the bad fixture, and neither plain safe
# pipelines, comments, quoted pipes nor whitelisted lines may fire.
self_test() {
  local dir=$1 got want
  cat >"$dir/bad.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
printf 'a\nb\n' | grep -q a
cmd | grep -m1 x
cmd | grep -l x
cmd | grep --quiet x
cmd | rg -q x
cmd | head
cmd | head -n1
cmd | sed -n '1q'
cmd | awk 'NR == 1 { print; exit }'
cmd | read -r line
cmd | head  # pipefail-ok:
x=$(git tag | head)
git tag | head; echo
{ cmd | head; }
x="$(git tag | head -n1)"
cmd | timeout 5 head
cmd | stdbuf -oL head
cmd | env FOO=bar head
cmd | nice -n 5 head
cmd | xargs -n 1 grep -q x
cmd | xargs -0 grep -q x
echo 'literal <<EOF'
cmd | head
FIXTURE
  cat >"$dir/good.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
grep -q pattern file
head -n1 file
printf 'a\nb\n' | grep -c a
printf 'a\nb\n' | grep -v a
notes=$(printf 'a\nb\n' | grep -E 'a$')
[[ $notes == *a* ]]
# printf 'x' | grep -q x
printf '%s' 'a | head'
cmd | awk '{exit_code=1}'
cmd | command -v head
cat <<'EOF'
printf 'a\nb\n' | grep -q a
EOF
cat <<-EOF
	printf 'a\nb\n' | head -n1
	EOF
cat <<EOF
printf 'a\nb\n' | grep -q a
EOF
cat <<"EOF"
printf 'a\nb\n' | grep -q a
EOF
FIXTURE
  cat >"$dir/whitelisted.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
printf 'x' | head -c1 >/dev/null  # pipefail-ok: only truncates a diagnostic dump
# pipefail-ok: previous-line marker
printf 'x' | head -c1 >/dev/null
FIXTURE
  cat >"$dir/bad.yml" <<'FIXTURE'
jobs:
  x:
    steps:
      - run: |
          printf 'a\nb\n' |
            grep -q a
      - run: git tag | head -n1
FIXTURE
  cat >"$dir/good.yml" <<'FIXTURE'
jobs:
  x:
    steps:
      - run: |
          out=$(printf 'a\nb\n')
          grep -q a <<<"$out"
      # - run: git tag | head -n1
FIXTURE
  cat >"$dir/mise.toml" <<'FIXTURE'
[tasks.bad]
run = '''
#!/usr/bin/env bash
set -euo pipefail
printf 'a\nb\n' | head -n1
'''
[tasks.no-pipefail]
run = '''
#!/bin/sh
set -eu
printf 'a\nb\n' | head -n1
'''
FIXTURE

  expect() {
    local fixture=$1 mode=$2 want=$3 stderr_file=$dir/stderr
    violations=0
    scan_stream "$fixture" "$mode" <"$fixture" >/dev/null 2>"$stderr_file"
    got=$violations
    if [[ $got -ne $want ]]; then
      fail "self-test: $fixture produced $got violation(s), want $want:
$(cat "$stderr_file")"
    fi
  }
  expect "$dir/bad.sh" 0 22
  expect "$dir/good.sh" 0 0
  expect "$dir/whitelisted.sh" 0 0
  expect "$dir/bad.yml" 1 2
  expect "$dir/good.yml" 1 0
  violations=0
  scan_stream "$dir/mise.toml" 1 < <(extract_mise_blocks "$dir/mise.toml") >/dev/null 2>"$dir/stderr"
  if [[ $violations -ne 1 ]]; then
    fail "self-test: mise.toml fixture produced $violations violation(s), want 1:
$(cat "$dir/stderr")"
  fi
  violations=0
}

fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
self_test "$fixture"

# Shell scripts that declare pipefail. /bin/sh scripts with plain `set -eu` are out
# of scope: they do not run with pipefail, so an early exit cannot flip the status.
while IFS= read -r file; do
  [[ ! -f $file ]] && continue
  IFS= read -r first <"$file" || true
  case $first in
  '#!'*bash* | '#!'*'/sh'* | '#!'*' sh'*) ;;
  *) continue ;;
  esac
  grep -Eq '^[[:space:]]*set[[:space:]]+[^#]*pipefail' "$file" || continue
  scan_stream "$file" <"$file"
done < <(git ls-files --cached --others --exclude-standard)

if [[ -f mise.toml ]]; then
  scan_stream mise.toml 1 < <(extract_mise_blocks mise.toml)
fi

shopt -s nullglob
for workflow in .github/workflows/*.yml .github/workflows/*.yaml \
  .github/actions/*/action.yml .github/actions/*/action.yaml; do
  scan_workflow_file "$workflow"
done

if ((violations > 0)); then
  fail "$violations pipeline stage(s) can close early under pipefail"
fi
printf 'pipefail pipeline check: ok\n'
