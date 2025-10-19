import os
import shutil
import subprocess
import tarfile
from contextlib import contextmanager
from pathlib import Path
from textwrap import dedent

import requests
from actions import core
from actions.github import get_githubkit

from .config import (
    ArchiveFile,
    CargoProject,
    Config,
    Container,
    ContainerProject,
    FileAsset,
    GoProject,
    PkgAsset,
)
from .utils import action


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


def pkg(
    version: str, arch: str, m: Path, cmd: str, *args: str, text: bool = False
) -> str:
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

    env = os.environ | {
        "IGNORE_OSVERSION": "yes",
        "PKG_CACHEDIR": "/tmp/cache",
        "ABI": f"FreeBSD:{major}:{'aarch64' if arch == 'arm64' else arch}",
    }

    if text:
        return subprocess.check_output(
            ["pkg", "--rootdir", m, cmd] + list(args), env=env, text=True
        ).strip()
    else:
        subprocess.check_call(["pkg", "--rootdir", m, cmd, "-y"] + list(args), env=env)
        return ""


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


def _calculate_dst(src: str, dst: str) -> str:
    if not dst.startswith("/"):
        raise ValueError(f"Invalid dst: {dst}")

    if dst.endswith("/"):
        dst = f"{dst}{src.rsplit('/', 1)[-1]}"

    return dst


def _extract_archive(m: Path, url: str, files: list[ArchiveFile]) -> str | None:
    t = m / "tmp/tarball"
    t.mkdir()

    core.info(f"Extracting {url}")

    with requests.get(url, stream=True) as r:
        r.raise_for_status()

        with tarfile.open(fileobj=r.raw, mode="r|*") as tar:
            tar.extractall(str(t))

    entrypoint = None

    for file in files:
        src = str(file.src)
        dst = str(file.dst)

        dir = src.endswith("/")
        src = src.rstrip("/")

        for s in t.glob(src):
            if s.is_dir() != dir:
                core.info(
                    f"{s.relative_to(t)} is a {'dir' if s.is_dir() else 'file'} but we want a {'dir' if dir else 'file'}"
                )
                continue

            d = _calculate_dst(s.name, dst)

            if not entrypoint and s.is_file() and s.stat().st_mode & 0o111 == 0o111:
                entrypoint = d

            d = m / d[1:]

            core.info(f"{s.relative_to(t)} -> {d.relative_to(m)}")

            d.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            s.replace(d)
            break
        else:
            raise ValueError(f"{src} not found")

    return entrypoint


@action
def build_container():
    gh = get_githubkit()

    project = core.get_input("project")
    version = core.get_input("version")
    name = core.get_input("container")

    config = Config.from_yaml("projects.yaml").root[project]
    archs = config.arch

    if isinstance(config, (CargoProject, GoProject)):
        assert project == name
        config = config.packages[name].container
    elif isinstance(config, ContainerProject):
        config = config.container
    else:
        raise TypeError("Unknown project type")

    assert isinstance(config, Container)

    latest = f"ghcr.io/cynix/{name}:latest"
    tagged = f"ghcr.io/cynix/{name}:{version}" if version else ""
    buildah("manifest", "create", latest)

    base = config.base or (
        "freebsd:runtime"
        if any(isinstance(x, PkgAsset) for x in config.assets)
        else "freebsd:static",
    )
    base = f"ghcr.io/cynix/{base}"

    subprocess.check_call(["podman", "pull", base])
    freebsd_version = subprocess.check_output(
        [
            "podman",
            "image",
            "inspect",
            '--format={{index .Annotations "org.freebsd.version"}}',
            base,
        ],
        text=True,
    ).strip()

    for arch in archs:
        core.info(f"Building arch: {arch}")

        triple = f"{arch.replace('amd64', 'x86_64').replace('arm64', 'aarch64')}-unknown-freebsd"

        with container(latest, base, arch) as (c, m):
            root = Path(name) / "root"
            if root.is_dir():
                core.info(f"Copying {root}")
                shutil.copytree(root, m, symlinks=True, dirs_exist_ok=True)

            if user := config.user:
                if "=" in user:
                    user, uid = user.rsplit("=", 1)
                    core.info(f"Creating {user} = {uid}")

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

            pkg_versions = {}

            # Install all packages in one go for efficiency
            if pkgs := [x.pkg for x in config.assets if isinstance(x, PkgAsset)]:
                core.info(f"Installing packages: {' '.join(pkgs)}")
                pkg(freebsd_version, arch, m, "install", *pkgs)
                pkg_versions = dict(
                    zip(
                        pkgs,
                        (
                            x.strip()
                            for x in pkg(
                                freebsd_version,
                                arch,
                                m,
                                "query",
                                "%v",
                                *pkgs,
                                text=True,
                            ).splitlines()
                        ),
                    )
                )

                shutil.rmtree(m / "var/db/pkg/repos")

                hints = set(["/lib", "/usr/lib", "/usr/local/lib"])

                for conf in (m / "usr/local/libdata/ldconfig").glob("*"):
                    with open(conf) as f:
                        hints.update(
                            x.strip()
                            for x in f.read().splitlines()
                            if x and not x.startswith("#")
                        )

                # Ensure dirs exist before running `ldconfig` on the host
                for d in hints:
                    os.makedirs(d, 0o755, exist_ok=True)

                subprocess.check_call(
                    ["ldconfig", "-f", m / "var/run/ld-elf.so.hints"] + sorted(hints)
                )

            for asset in config.assets:
                if isinstance(asset, FileAsset):
                    url, ver = asset.resolve(
                        project=project,
                        version=version,
                        package=name,
                        arch=arch,
                        triple=triple,
                    )
                    dst = _calculate_dst(url, asset.dst or f"/usr/local/{name}/")

                    core.info(f"{url} -> {dst}")

                    with requests.get(url, stream=True) as r:
                        r.raise_for_status()

                        out = m / dst[1:]
                        out.parent.mkdir(parents=True, exist_ok=True)

                        with open(out, "wb") as f:
                            shutil.copyfileobj(r.raw, f)

                        if ver and not tagged:
                            core.info(f"Deduced image version from file: {url} = {ver}")
                            tagged = f"ghcr.io/cynix/{name}:{ver}"

                        if not config.entrypoint:
                            core.info(f"Deduced entrypoint: {dst}")
                            os.chmod(out, 0o755)
                            config.entrypoint = dst
                elif isinstance(asset, PkgAsset):
                    ver = pkg_versions[asset.pkg]
                    buildah(
                        "config",
                        f"--annotation=org.freebsd.pkg.{asset.pkg}.version={ver}",
                        c,
                    )

                    if not tagged:
                        core.info(
                            f"Deduced image version from package: {asset.pkg} = {ver}"
                        )
                        tagged = f"ghcr.io/cynix/{name}:{ver}"

                    if not config.entrypoint:
                        core.info(
                            f"Deduced entrypoint from package: /usr/local/bin/{asset.pkg}"
                        )
                        config.entrypoint = f"/usr/local/bin/{asset.pkg}"
                else:
                    url, ver = asset.resolve(
                        gh=gh,
                        project=project,
                        version=version,
                        package=name,
                        arch=arch,
                        triple=triple,
                    )
                    entrypoint = _extract_archive(
                        m, url, asset.files or [ArchiveFile(src=f"**/{name}")]
                    )

                    if ver and not tagged:
                        tagged = f"ghcr.io/cynix/{name}:{ver}"

                    if not config.entrypoint:
                        core.info(
                            f"Deduced entrypoint from archive: {url} = {entrypoint}"
                        )
                        config.entrypoint = entrypoint

            if (m / "usr/local/sbin").is_dir():
                os.chmod(m / "usr/local/sbin", 0o711)

            if config.script:
                core.info("Running build script")
                subprocess.run(
                    ["sh", "-ex"], cwd=m, input=config.script, text=True, check=True
                )

            if isinstance(config.entrypoint, str):
                config.entrypoint = [config.entrypoint]
            assert isinstance(config.entrypoint, list)
            entrypoint = ",".join(f'"{x}"' for x in config.entrypoint)

            cmd = ["config", f"--entrypoint=[{entrypoint}]", "--cmd="] + [
                f"--env={k}={v}" for k, v in config.env.items()
            ]

            if user:
                cmd.append(f"--user={user}:{user}")

            buildah(*cmd, c)

    buildah("manifest", "push", "--all", latest, f"docker://{latest}")

    if tagged:
        buildah("manifest", "push", "--all", latest, f"docker://{tagged}")
