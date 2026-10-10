#!/usr/bin/env bash
# 便携版 CI 的诊断辅助。
#
# 为什么要有这个文件
# ------------------
# 前两次真实失败都死在同一类问题上：某个「服务端行为」与我们的假设不符
# （upload-artifact 默认丢点文件、download-artifact 嗅探到 zip 就解包），
# 而日志里恰好没有记录那个假设的现场，只能靠事后一层层猜。
#
# 结论：把假设在日志里变成事实。所有输出都以 [DIAG] 开头，日志里
#   grep '\[DIAG\]'
# 就能一次拿到全部诊断点；出错时先看最后一段 [DIAG] 即可定位到具体阶段。
#
# 两种用法：
#   source scripts/ci-diag.sh          # 在别的脚本里调用函数
#   bash   scripts/ci-diag.sh <cmd>    # 直接当命令用（工作流里用这种）

set -uo pipefail

# 统一的标记：grep 得动、看得懂、能定位
diag_banner() {
    printf '\n=== [DIAG] %s ===\n' "$*"
}

# 运行环境事实：出问题时第一眼要看的东西
diag_env() {
    diag_banner "env"
    echo "[DIAG] os: $(uname -srm 2>/dev/null || echo unknown)"
    echo "[DIAG] pwd: $(pwd)"
    echo "[DIAG] disk: $(df -h . 2>/dev/null | tail -1 | tr -s ' ')"
    local v val
    for v in GITHUB_JOB GITHUB_EVENT_NAME GITHUB_REF GITHUB_SHA RUNNER_OS \
             GO_VERSION NODE_VERSION DUCKDB_VERSION WAILS_CLI_VERSION \
             PORTABLE_RELEASE_TAG RELEASE_TAG_INPUT; do
        eval "val=\${$v-}"
        echo "[DIAG] $v=${val:-<unset>}"
    done
    local t p
    for t in go node npm zip unzip tar sha256sum file stat curl gh python3; do
        p="$(command -v "$t" 2>/dev/null || true)"
        echo "[DIAG] tool $t -> ${p:-MISSING}"
    done
}

# 目录现场：文件数、体积、树。含隐藏文件（find 默认就会列出来）
diag_tree() {
    local dir="$1" depth="${2:-2}" n
    diag_banner "tree $dir (maxdepth $depth)"
    if [ ! -e "$dir" ]; then
        echo "[DIAG] $dir DOES NOT EXIST"
        return 0
    fi
    n="$(find "$dir" -type f 2>/dev/null | wc -l | tr -d ' ')"
    echo "[DIAG] $dir: ${n} file(s), $(du -sh "$dir" 2>/dev/null | cut -f1)"
    find "$dir" -maxdepth "$depth" -printf '%y %10s  %p\n' 2>/dev/null | sort -k3 | head -60
}

# 隐藏文件审计。upload-artifact 的 include-hidden-files 默认 false，
# 会把点开头的文件静默丢掉，而上传步骤本身报「成功」——踩过两次了。
diag_dotfiles() {
    local dir="$1" flag="${2:-}" n
    n="$(find "$dir" -name '.*' -type f 2>/dev/null | wc -l | tr -d ' ')"
    echo "[DIAG] dotfiles under $dir: $n"
    find "$dir" -name '.*' -type f 2>/dev/null | head -20 | sed 's/^/[DIAG]   /'
    if [ "$n" -gt 0 ] && [ "$flag" != "true" ]; then
        echo "[DIAG] !! $n hidden file(s) will be DROPPED - the upload step needs include-hidden-files: true"
    fi
}

# 上传前的清单：这一步到底会送出什么。放在产出步骤的末尾调用，
# 与随后的 upload-artifact 相邻，出问题时能立刻分辨是「没生成」还是「传丢了」。
diag_upload() {
    local dir="$1" name="$2" hidden="${3:-false}"
    diag_banner "upload '$name' <- $dir"
    diag_tree "$dir" 2
    diag_dotfiles "$dir" "$hidden"
}

# 下载后的清单：download-artifact 的落点曾经坑过我们（嗅探 zip 就解包），
# 所以下载之后必须把「实际落成什么样」打出来，而不是假设它落在哪。
diag_download() {
    local dir="$1" name="$2"
    diag_banner "downloaded '$name' -> $dir"
    diag_tree "$dir" 2
    diag_dotfiles "$dir" "true"
}

# 失败现场转储：任何 job 失败都跑这个，把磁盘状态摊开。
# 同时写进 job summary —— 失败时最该先看的就是现场，而 summary 在页面上直接可见，
# 不必先下载整份日志。
diag_failure() {
    local dir="${1:-.}"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
        {
            printf '```\n'
            diag_banner "FAILURE DUMP (job=${GITHUB_JOB:-?})"
            diag_env
            diag_tree "$dir" 2
            diag_dotfiles "$dir" "true"
            printf '```\n'
        } 2>&1 | tee -a "$GITHUB_STEP_SUMMARY"
    else
        diag_banner "FAILURE DUMP (job=${GITHUB_JOB:-?})"
        diag_env
        diag_tree "$dir" 2
        diag_dotfiles "$dir" "true"
    fi
}

# 产物清单：把「我们到底送出了什么」写进 job summary。
# 前两轮的失败形态都是「构建全绿但产物是错的」，所以清单必须出现在页面上，
# 而不是埋在几万行日志中间 —— 折叠块里一眼能看出少了哪个文件。
diag_manifest() {
    local dir="$1" name="${2:-$1}" n size
    n="$(find "$dir" -type f 2>/dev/null | wc -l | tr -d ' ')"
    size="$(du -sh "$dir" 2>/dev/null | cut -f1)"
    echo "[DIAG] manifest $name: $n file(s), ${size:-?}"
    [ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
    {
        printf '\n<details><summary>%s — %s file(s), %s</summary>\n\n```\n' \
            "$name" "$n" "${size:-?}"
        find "$dir" -type f -printf '%10s  %p\n' 2>/dev/null | sort -k2 | head -600
        printf '```\n\n</details>\n'
    } >> "$GITHUB_STEP_SUMMARY"
    echo "[DIAG] manifest written to job summary ($GITHUB_STEP_SUMMARY)"
}

# ── CLI 入口 ────────────────────────────────────────────────────────────────
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    cmd="${1:-}"
    shift || true
    case "$cmd" in
        env)      diag_env ;;
        tree)     diag_tree "$@" ;;
        dotfiles) diag_dotfiles "$@" ;;
        upload)   diag_upload "$@" ;;
        download) diag_download "$@" ;;
        failure)  diag_failure "$@" ;;
        manifest) diag_manifest "$@" ;;
        "")
            echo "usage: $0 {env|tree <dir> [depth]|dotfiles <dir> [true]|upload <dir> <name> <true|false>|download <dir> <name>|manifest <dir> [name]|failure [dir]}" >&2
            exit 2 ;;
        *)
            echo "unknown diagnostic command: $cmd" >&2
            exit 2 ;;
    esac
fi
