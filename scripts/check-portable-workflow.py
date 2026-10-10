#!/usr/bin/env python3
"""便携版工作流的静态不变量检查（在 preflight job 里跑，几秒钟）。

前两轮真实失败都不是代码写错，而是「工作流自身的约定被违反」：

  * package-portable 上传的 zip，publish-release 却按目录下载 —— 名字对上了，
    但语义没对上；更早一版是名字就没对上。
  * portable-dist 上传时没开 include-hidden-files，.env 被静默丢掉，
    而下游断言 .env 存在 —— 生产者和消费者的约定不一致。

这类问题不需要跑十分钟的构建才能发现，读一遍 YAML 就能发现。把它们写成
断言，失败信息里直接说清「谁和谁对不上」，比事后翻日志猜快得多。

退出码：0 = 全部通过；1 = 有硬性不变量被违反；2 = 检查器自身无法运行。
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - runner 上几乎总是有
    print("::error::[preflight/check] PyYAML 不可用，先执行 python3 -m pip install pyyaml")
    sys.exit(2)

WF = Path(__file__).resolve().parent.parent / ".github/workflows/build-windows-portable.yml"
# 每个 ::error:: 后面必须紧跟 [job/stage] 标签，否则日志里定位不到阶段
ERROR_TAG = re.compile(r'::error::\[[a-z0-9-]+/[a-z0-9-]+\]')
V_TAG = re.compile(r"^v\d+\.\d+\.\d+$")

failures: list[str] = []
checks = 0


def check(ok: bool, msg: str, hint: str = "") -> None:
    global checks
    checks += 1
    if ok:
        print(f"[check] ok   {msg}")
        return
    line = f"{msg}" + (f" -- {hint}" if hint else "")
    failures.append(line)
    print(f"::error::[preflight/check] {line}")


def steps_of(job: dict):
    return job.get("steps") or []


def upload_name(step: dict) -> str | None:
    """产物名：显式 name 优先；否则单文件 path 取文件名（实跑日志核实过）。"""
    w = step.get("with") or {}
    if w.get("name"):
        return str(w["name"])
    path = str(w.get("path", "")).strip()
    if not path or any(c in path for c in "*?[]"):
        return None
    if path.endswith("/"):
        return None
    return Path(path).name


def main() -> int:
    wf = yaml.safe_load(WF.read_text())
    jobs = wf["jobs"]

    # ── 1. 产物名：每个下载都必须有对应的上传 ────────────────────────────
    produced: dict[str, str] = {}
    for jn, job in jobs.items():
        for s in steps_of(job):
            if "upload-artifact@" not in (s.get("uses") or ""):
                continue
            nm = upload_name(s)
            if nm:
                produced[nm] = jn

    for jn, job in jobs.items():
        for s in steps_of(job):
            if "download-artifact@" not in (s.get("uses") or ""):
                continue
            nm = str((s.get("with") or {}).get("name", ""))
            check(
                nm in produced,
                f"{jn}: download-artifact '{nm}' 有生产者",
                f"没有任何 upload-artifact 产出这个名字（现有：{sorted(produced)}）",
            )

    # ── 2. 点文件：所有上传都必须显式开启 include-hidden-files ───────────
    # 默认 false 会静默丢弃 .env 这类文件，而上传步骤依然报成功。
    # 我们的产物里确实有 .env，与其逐处判断，不如要求处处开启：代价为零。
    for jn, job in jobs.items():
        for s in steps_of(job):
            if "upload-artifact@" not in (s.get("uses") or ""):
                continue
            w = s.get("with") or {}
            hidden = str(w.get("include-hidden-files", "false")).lower() == "true"
            check(
                hidden,
                f"{jn}: upload-artifact '{upload_name(s)}' 开了 include-hidden-files",
                "默认 false 会静默丢掉 .env 等点文件，上传却报成功",
            )

    # ── 3. Release 资产清单：发布的和打包的必须是同一套 ──────────────────
    # 打包在 "Build release assets" 步骤、发布在 "Publish (create or overwrite)"
    # 步骤 —— 两个步骤看的是同一份文件清单，所以常量要在整个 job 里找。
    pub = None
    jobtext = ""
    for jn, job in jobs.items():
        if jn == "publish-release":
            jobtext = "\n".join((s.get("run") or "") for s in steps_of(job))
        for s in steps_of(job):
            if (s.get("name") or "") == "Publish (create or overwrite)":
                pub = s.get("run") or ""

    check(pub is not None, "找到 publish-release 的发布步骤")
    m = re.search(r'ARCHIVE="([^"]+)"', jobtext)
    check(
        m is not None,
        "publish-release 里能解析出 ARCHIVE 常量",
        "打包步骤与发布步骤必须共用同一个常量，否则两边会漂移",
    )
    archive = m.group(1) if m else ""
    arr = re.search(r"ASSETS=\((.*?)\n\s*\)", pub or "", re.S)
    check(arr is not None, "发布步骤里能解析出 ASSETS 数组")
    listed = sorted(re.findall(r"release-assets/(\S+)", arr.group(1) if arr else ""))
    expected = sorted([f"{archive}.zip", f"{archive}.zip.sha256", "WeKnora-Lite.exe"])
    check(
        listed == expected,
        "ASSETS 数组与打包步骤产出的文件一致",
        f"数组={listed} 期望={expected}",
    )
    for a in listed:
        if a == "WeKnora-Lite.exe":
            continue  # 由 download-artifact 提供
        check(
            a in jobtext,
            f"资产 {a} 确实由 publish-release 生成",
            "ASSETS 里列了它，却没有任何步骤产出它",
        )
    check(
        "${ARCHIVE}.zip.sha256" in jobtext,
        "校验文件名由 ARCHIVE 推导（<ARCHIVE>.zip.sha256）",
        "校验文件必须与 zip 同名加 .sha256，否则用户 sha256sum -c 找不到目标",
    )

    # ── 4. 固定 Release 标签不能命中 v*.*.* 触发器 ────────────────────────
    tag = str((wf.get("env") or {}).get("PORTABLE_RELEASE_TAG", ""))
    check(
        not V_TAG.match(tag),
        f"PORTABLE_RELEASE_TAG='{tag}' 不会命中 push: tags: ['v*.*.*']",
        "v*.*.* 形式的标签会让 gh release create 反过来再触发本工作流",
    )
    check(
        bool(pub) and "v*.*.*" in pub,
        "发布步骤保留了 v*.*.* 的运行时防呆",
        "静态检查之外，运行时也要挡住手工传入的 v*.*.* 标签",
    )

    # ── 5. 每个 job 都要有失败现场转储 ────────────────────────────────────
    # 失败时最贵的成本是「不知道磁盘上当时是什么」，所以每个 job 都必须
    # 有一个 if: failure() 的步骤把现场摊开。
    for jn, job in jobs.items():
        dump = [
            s for s in steps_of(job)
            if "failure()" in str(s.get("if", "")) and "ci-diag.sh failure" in str(s.get("run", ""))
        ]
        check(bool(dump), f"{jn}: 有 if: failure() 的现场转储步骤")

    # ── 6. 每个 ::error:: 都带阶段标签 ───────────────────────────────────
    for jn, job in jobs.items():
        for s in steps_of(job):
            run = s.get("run")
            if not run:
                continue
            for ln in run.splitlines():
                if "::error::" in ln:
                    check(
                        bool(ERROR_TAG.search(ln)),
                        f"{jn}/{(s.get('name') or '?')[:28]}: 错误信息带 [阶段] 标签",
                        f"缺标签：{ln.strip()[:90]}",
                    )

    # ── 7. preflight 存在，且每个 job 都受它把关 ─────────────────────────
    check("preflight" in jobs, "存在 preflight job（静态检查跑在构建之前）")
    if "preflight" in jobs:
        pr = steps_of(jobs["preflight"])
        check(
            any("check-portable-workflow.py" in str(s.get("run", "")) for s in pr),
            "preflight 会执行 check-portable-workflow.py",
        )

        def needs_of(jn: str) -> list[str]:
            n = jobs[jn].get("needs")
            if n is None:
                return []
            return [n] if isinstance(n, str) else list(n)

        def gated(jn: str, seen: set[str] | None = None) -> bool:
            """直接或间接依赖 preflight —— 只看直接依赖会漏掉链式把关。"""
            seen = seen or set()
            if jn == "preflight":
                return True
            if jn in seen:
                return False
            seen.add(jn)
            return any(gated(d, seen) for d in needs_of(jn))

        for jn in jobs:
            if jn == "preflight":
                continue
            check(
                gated(jn),
                f"{jn}: 直接或间接依赖 preflight",
                "否则静态检查挡不住它，约定不一致会一路跑到构建结束",
            )

    # ── 8. 每个 job 都要有「正常路径」上的诊断点 ─────────────────────────
    # 只有失败转储是不够的：它只在失败后出现，而我们要的是成功路径上也留下
    # 现场 —— 因为「构建全绿但产物是错的」正是前两轮的失败形态。
    for jn, job in jobs.items():
        points = [
            s for s in steps_of(job)
            if "ci-diag.sh" in str(s.get("run", "")) and "failure()" not in str(s.get("if", ""))
        ]
        check(
            bool(points),
            f"{jn}: 有正常路径上的 [DIAG] 诊断点",
            "失败转储不能代替成功路径的现场记录",
        )

    # ── 9. 每个 bash run 块都能被解析 ────────────────────────────────────
    # 语法错误是纯静态问题，却要等到对应 job 跑起来才炸（前面几个 job 白跑）。
    # 这里用 bash -n 只做解析，不执行任何东西。
    # 判定用哪个 shell：步骤上的 shell > job 的 defaults.run.shell > 平台默认
    # （ubuntu 是 bash，windows 是 pwsh）。非 bash 的块跳过。
    skipped = 0
    for jn, job in jobs.items():
        job_shell = ((job.get("defaults") or {}).get("run") or {}).get("shell", "")
        is_win = "windows" in str(job.get("runs-on", ""))
        for s in steps_of(job):
            if "run" not in s:
                continue
            sh = str(s.get("shell") or job_shell or ("pwsh" if is_win else "bash"))
            if "bash" not in sh:
                skipped += 1
                continue
            # GitHub 先把 ${{ ... }} 替换掉再交给 shell，所以这里也要替换：
            # 否则表达式本身会被当成 bash 语法来解析（${{ x }} 是非法展开）。
            src = re.sub(r"\$\{\{.*?\}\}", "X", str(s["run"]), flags=re.S)
            r = subprocess.run(
                ["bash", "-n"], input=src, text=True, capture_output=True
            )
            detail = ""
            if r.returncode != 0:
                err = (r.stderr or "").strip().splitlines()
                detail = err[-1] if err else f"exit {r.returncode}"
            check(
                r.returncode == 0,
                f"{jn}/{(s.get('name') or '?')[:28]}: run 块 bash -n 通过",
                detail,
            )
    if skipped:
        print(f"[check]     （{skipped} 个非 bash run 块已跳过）")

    # ── 10. 便携化的「接线」不能断 ───────────────────────────────────────
    # 这一组守的是 system-admin 提权的三处接线。它们都是普通文件里的一行，
    # 构建全绿也照样能被上游合并悄悄改掉 —— 直到用户解压双击，发现
    # 「设置 → 系统管理」整组页面进不去（真实发生过一次）。
    repo = WF.resolve().parents[2]

    def read(rel: str) -> str:
        p = repo / rel
        return p.read_text(encoding="utf-8", errors="replace") if p.exists() else ""

    check(
        "runtime.RunStartupBootstrap(" in read("cmd/desktop/main.go"),
        "cmd/desktop/main.go: 调用 runtime.RunStartupBootstrap",
        "桌面壳不调这个钩子，auto-setup 建出的账号就永远是普通用户，"
        "而唯一能提权的接口本身就在 system-admin 守卫组里 —— 便携版没有第二条路",
    )
    check(
        "runtime.RunStartupBootstrap(" in read("cmd/server/main.go"),
        "cmd/server/main.go: 调用 runtime.RunStartupBootstrap",
        "Docker 部署靠它提权，删掉等于平台管理功能整体失效",
    )

    launcher = read("scripts/Start-Portable.bat.template")
    m = re.search(r'^set "WEKNORA_BOOTSTRAP_SYSTEM_ADMIN_EMAIL=([^"]*)"', launcher, re.M)
    check(
        bool(m and m.group(1).strip()),
        "启动器模板: 导出非空的 WEKNORA_BOOTSTRAP_SYSTEM_ADMIN_EMAIL",
        "没有它，便携版的第一个账号永远不是 system admin",
    )

    # 启动器点名的账号必须就是 auto-setup 建出来的那个，否则提权打在一个
    # 不存在的用户上：日志里只有一行 warning，然后什么都不发生。
    am = re.search(r'const\s+defaultEmail\s*=\s*"([^"]+)"', read("internal/handler/auth.go"))
    check(
        bool(m and am and m.group(1).strip() == am.group(1)),
        "启动器点名的邮箱 == internal/handler/auth.go 里 auto-setup 的 defaultEmail",
        f"启动器={m.group(1) if m else '?'} auto-setup={am.group(1) if am else '?'}",
    )

    # smoke test 必须真断言提权成功，且邮箱值从启动器模板里取 ——
    # 不允许在 YAML 里再写一遍，写两遍早晚漂移。
    smoke = [
        s for s in steps_of(jobs["package-portable"])
        if "weknora-diag" in str(s.get("run", ""))
    ]
    smoke_run = str(smoke[0]["run"]) if smoke else ""
    check(
        "DIAG_BOOTSTRAP_CHECK=1" in smoke_run,
        "package-portable/smoke: 用 DIAG_BOOTSTRAP_CHECK=1 真跑一遍提权",
        "只 build 不 assert 的话，提权坏掉也能全绿发版",
    )
    check(
        "Start-Portable.bat.template" in smoke_run and "BOOTSTRAP OK" in smoke_run,
        "package-portable/smoke: 邮箱取自启动器模板并断言 BOOTSTRAP OK",
        "两处各写一遍邮箱值，早晚会漂移",
    )

    print()
    print(f"[check] {checks} 项断言，{len(failures)} 项失败")
    if failures:
        print("::error::[preflight/check] 静态不变量检查未通过，先修 YAML 再跑构建")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("[check] 全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
