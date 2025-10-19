#!/usr/bin/env -S uv run --no-project --script
# /// script
# requires-python = ">=3.13"
# dependencies = [
#     "PyGithub",
#     "requests",
#     "ruamel.yaml",
# ]
# ///

import os
import re
import shutil
import subprocess
import sys
import tarfile
from contextlib import contextmanager
from fnmatch import fnmatch
from pathlib import Path
from textwrap import dedent
from typing import Any

import requests
from github import Github
from ruamel.yaml import YAML


def buildah(*args: str, text: bool = True) -> str:
    cmd = ["buildah"]
    cmd.extend(args)

    if text:
        return subprocess.check_output(cmd, text=True).strip()
    else:
        subprocess.check_call(cmd)
        return ""


def pw(m: Path, *args: str):
    subprocess.check_call(["pw", "-R", m] + list(args))


def pkg(version, arch: str, m: Path, cmd: str, *args: str, text: bool = False) -> str:
    major, minor, *_ = version.split("p")[0].split(".")

    with open("/usr/local/etc/pkg/repos/FreeBSD.conf", "w") as f:
        print(
            dedent(f"""
            FreeBSD: {{
              url: "pkg+https://pkg.FreeBSD.org/${{ABI}}/latest"
            }}
            FreeBSD-base: {{
              url: "pkg+https://pkg.FreeBSD.org/${{ABI}}/base_release_{minor}",
              mirror_type: "srv",
              signature_type: "fingerprints",
              fingerprints: "/usr/share/keys/pkg",
              enabled: yes
            }}
            FreeBSD-kmods: {{
              enabled: no
            }}
            """),
            file=f,
        )

    env = dict(os.environ)
    env.update(
        IGNORE_OSVERSION="yes",
        PKG_CACHEDIR="/tmp/cache",
        ABI=f"FreeBSD:{major}:{'aarch64' if arch == 'arm64' else arch}",
    )

    if text:
        return subprocess.check_output(
            ["pkg", "--rootdir", m, cmd] + list(args), env=env, text=True
        ).strip()
    else:
        subprocess.check_call(["pkg", "--rootdir", m, cmd, "-y"] + list(args), env=env)
        return ""


def get_version(version: str | dict[str, str] | None) -> str | None:
    if not version:
        return None

    if isinstance(version, str):
        return version

    body = requests.get(version["url"]).text
    if "regex" not in version:
        return body

    if m := re.search(version["regex"], body):
        return m.group("version")

    return None


@contextmanager
def container(manifest: str, base: str, arch: str):
    c = ""
    m = ""

    try:
        c = buildah("from", f"--arch={arch}", f"ghcr.io/cynix/{base}", text=True)
        m = buildah("mount", c, text=True)
        yield (c, Path(m))
    finally:
        if m:
            buildah("unmount", c)
            buildah("commit", f"--manifest={manifest}", "--rm", c)
        elif c:
            buildah("rm", c)


def main(name: str, config: dict[str, Any]) -> None:
    latest = f"ghcr.io/cynix/{name}:latest"
    tagged = ""
    buildah("manifest", "create", latest)

    base = config.get(
        "base", "freebsd:runtime" if "pkg" in config else "freebsd:static"
    )

    image = f"ghcr.io/cynix/{base}"
    subprocess.check_call(["podman", "pull", image])
    version = subprocess.check_output(
        [
            "podman",
            "image",
            "inspect",
            '--format={{index .Annotations "org.freebsd.version"}}',
            image,
        ],
        text=True,
    ).strip()

    for arch in config.get("arch", ["amd64", "arm64"]):
        triple = f"{arch.replace('amd64', 'x86_64').replace('arm64', 'aarch64')}-unknown-freebsd"

        with container(latest, base, arch) as (c, m):
            if os.path.isdir(name):
                shutil.copytree(name, m, symlinks=True, dirs_exist_ok=True)

            if user := config.get("user"):
                if "=" in user:
                    user, uid = user.split("=")
                    pw(m, "groupadd", "-n", user, "-g", uid)
                    pw(
                        m,
                        "useradd",
                        "-n",
                        user,
                        "-u",
                        uid,
                        "-g",
                        user,
                        "-d",
                        "/nonexistent",
                        "-s",
                        "/sbin/nologin",
                    )

            if pkgs := config.get("pkg"):
                pkg(version, arch, m, "install", *pkgs)
                versions = {
                    p: pkg(version, arch, m, "query", "%v", p, text=True) for p in pkgs
                }
                tagged = f"ghcr.io/cynix/{name}:{versions[pkgs[0]]}"

                for p in pkgs:
                    buildah(
                        "config",
                        f"--annotation=org.freebsd.pkg.{p}.version={versions[p]}",
                        c,
                    )

                shutil.rmtree(m / "var/db/pkg/repos")

                hints = set(["/lib", "/usr/lib", "/usr/local/lib"])

                for conf in (m / "usr/local/libdata/ldconfig").glob("*"):
                    with open(conf) as f:
                        hints.update(x for x in f.read().splitlines() if x)

                for d in hints:
                    os.makedirs(d, 0o755, exist_ok=True)

                subprocess.check_call(
                    ["ldconfig", "-f", m / "var/run/ld-elf.so.hints"] + sorted(hints)
                )

                if "entrypoint" not in config:
                    config["entrypoint"] = f"/usr/local/bin/{pkgs[0]}"

            for tarball in config.get("tarball", []):
                if repo := tarball.get("repo"):
                    binary = tarball.get("binary", repo.split("/")[1])
                    glob = tarball["glob"].format(arch=arch, triple=triple)

                    release = Github().get_repo(repo).get_latest_release()
                    tag = release.tag_name

                    for asset in release.get_assets():
                        if fnmatch(asset.browser_download_url, glob):
                            url = asset.browser_download_url
                            break
                    else:
                        raise RuntimeError(f"{glob} not found in {release.assets}")
                else:
                    urls, binary = tarball["url"].split("#")
                    tag = get_version(tarball.get("version"))
                    url = urls.format(arch=arch, triple=triple, version=tag)

                if "entrypoint" not in config:
                    config["entrypoint"] = f"/usr/local/bin/{binary}"

                subprocess.check_call(["fetch", "-o", "/tmp/tarball", url])

                with tarfile.open("/tmp/tarball") as t:
                    while member := t.next():
                        if not member.isfile() or (member.mode & 0o111) != 0o111:
                            continue

                        if os.path.basename(member.name) == binary:
                            bin = m / "usr/local/bin" / binary
                            os.makedirs(bin.parent, 0o755, exist_ok=True)

                            with open(bin, "wb") as dst:
                                src = t.extractfile(member)
                                assert src
                                shutil.copyfileobj(src, dst)

                            os.chmod(bin, 0o755)
                            break
                    else:
                        raise RuntimeError(f"{binary} not found in {url}")

                buildah("config", f"--annotation=org.freebsd.bin.{binary}.url={url}", c)

                if tag:
                    buildah(
                        "config",
                        f"--annotation=org.freebsd.bin.{binary}.version={tag}",
                        c,
                    )
                    tagged = f"ghcr.io/cynix/{name}:{tag}"

            for file in config.get("file", []):
                url = file["url"]
                filename = url.split("/")[-1]

                with requests.get(url, stream=True) as r:
                    r.raise_for_status()

                    dst = m / f"usr/local/{name}" / filename
                    os.makedirs(dst.parent, exist_ok=True)

                    with open(dst, "wb") as f:
                        shutil.copyfileobj(r.raw, f)

                buildah(
                    "config", f"--annotation=org.freebsd.file.{filename}.url={url}", c
                )

                if tag := get_version(file.get("version")):
                    buildah(
                        "config",
                        f"--annotation=org.freebsd.file.{filename}.version={tag}",
                        c,
                    )
                    tagged = f"ghcr.io/cynix/{name}:{tag}"

            if (m / "usr/local/sbin").is_dir():
                os.chmod(m / "usr/local/sbin", 0o711)

            if script := config.get("script"):
                subprocess.run(
                    ["sh", "-e"], check=True, input=script.format(root=m), text=True
                )

            if isinstance(config["entrypoint"], str):
                config["entrypoint"] = [config["entrypoint"]]

            entrypoint = ",".join(f'"{x}"' for x in config["entrypoint"])
            cmd = ["config", f"--entrypoint=[{entrypoint}]", "--cmd="] + [
                f"--env={k}={v}" for k, v in config.get("env", {}).items()
            ]

            if user:
                cmd.append(f"--user={user}:{user}")

            buildah(*cmd, c)

    buildah("manifest", "push", "--all", latest, f"docker://{latest}")

    if tagged:
        buildah("manifest", "push", "--all", latest, f"docker://{tagged}")


if __name__ == "__main__":
    name = sys.argv[1]

    with open("containers.yaml") as y:
        main(name, YAML().load(y)[name])
