# shellcheck shell=sh
# 供 source：解析 uv 受管的 Python 解释器。POSIX sh 语法，bash 与 /bin/sh 的脚本都能 source。
#
#   . "$ROOT/scripts/lib/uv-python.sh"
#   PY=$(lo_uv_python) || exit 1
#   "$PY" -c 'print(1)'
#
# 成功时只在 stdout 输出解释器的绝对路径；uv 的安装输出和所有错误都走 stderr。
# 约束（门禁脚本共用，不要在调用方各自放宽）：
#   - 解释器一律由 uv 受管，不用系统 python3、不用 pip，也不接受 PYTHON 之类的覆盖；
#   - uv 缺失是失败（return 1），不是 SKIP：CI 里 SKIP 会让门禁悄悄放行；
#   - 路径必须落在 `uv python dir` 之下：版本请求写成路径等形式时，不能借 uv python find 落回系统 Python；
#   - 版本由 LO_SCRIPTS_PYTHON_VERSION 覆盖，默认 3.13；
#   - --no-bin：不往 ~/.local/bin 放 python3.X，门禁不改宿主机 PATH 上的命令。

lo_uv_python() {
  _lo_py_version=${LO_SCRIPTS_PYTHON_VERSION:-3.13}
  if ! command -v uv >/dev/null 2>&1; then
    echo "FAIL: uv is required (managed Python $_lo_py_version); run through mise so tools.uv is on PATH" >&2
    return 1
  fi
  if ! uv python install --managed-python --no-bin "$_lo_py_version" >&2; then
    echo "FAIL: uv could not install managed Python $_lo_py_version" >&2
    return 1
  fi
  if ! _lo_py_root=$(uv python dir); then
    echo "FAIL: uv could not report its managed Python directory" >&2
    return 1
  fi
  # 托管目录必须是非空绝对路径：空串会让下面的前缀检查退化成 "/*"，任何绝对路径（含 /usr/bin/python3）都能通过。
  case $_lo_py_root in
    /?*) ;;
    *)
      echo "FAIL: uv reported an invalid managed Python directory: '$_lo_py_root'" >&2
      return 1
      ;;
  esac
  if ! _lo_py_path=$(uv python find --managed-python "$_lo_py_version"); then
    echo "FAIL: uv could not locate managed Python $_lo_py_version" >&2
    return 1
  fi
  case $_lo_py_path in
    "$_lo_py_root"/*) ;;
    *)
      echo "FAIL: uv returned a Python outside its managed directory $_lo_py_root: $_lo_py_path" >&2
      return 1
      ;;
  esac
  if [ ! -x "$_lo_py_path" ]; then
    echo "FAIL: uv returned a non-executable Python path: $_lo_py_path" >&2
    return 1
  fi
  printf '%s\n' "$_lo_py_path"
}
