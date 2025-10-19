import os
import re
import subprocess
from collections.abc import Callable
from contextlib import contextmanager
from fnmatch import fnmatchcase
from functools import partial, wraps
from pathlib import Path

from actions import core
from githubkit import GitHub
from githubkit.versions.latest.models import Release
from packaging.version import Version as PackagingVersion
from rich.console import Console

console = Console(color_system="truecolor", width=120)


@contextmanager
def action_group(name: str):
    core.start_group(name)
    try:
        yield
    finally:
        core.end_group()


def action(f):
    @wraps(f)
    def g():
        try:
            f()
        except Exception as e:
            with action_group("Stacktrace"):
                console.print_exception(show_locals=True)
            core.set_failed(e)
            return 1

    return g


def dockcross(cmd: list[str], **kw):
    prefix = [
        "docker",
        "run",
        "--rm",
        "--pull=always",
        f"--volume={Path(kw.pop('cwd', '.')).resolve()}:/work",
        "--env=BUILDER_USER=runner",
        "--env=BUILDER_GROUP=runner",
        f"--env=BUILDER_UID={os.getuid()}",
        f"--env=BUILDER_GID={os.getgid()}",
    ]

    if arch := kw.pop("arch", None):
        prefix.append(f"--env=FREEBSD_ARCH={arch.value}")

    prefix.extend(f"--env={k}={v}" for k, v in kw.pop("env", {}).items())

    prefix.append("ghcr.io/cynix/dockcross-freebsd:latest")
    subprocess.check_call(prefix + cmd, **kw)


def apply_patches(project: str):
    for patch in Path(".").glob(f"{project}/*.patch"):
        with action_group(f"Applying {patch}"):
            with open(patch) as f:
                subprocess.check_call(["patch", "-p1"], stdin=f, cwd="src")


class SemVer:
    raw: str | None = None
    semver: PackagingVersion | None = None

    def __init__(self, version: str | None):
        self.raw = version

        if version:
            try:
                self.semver = PackagingVersion(version.lstrip("v"))
                self.raw = version.lstrip("v")
            except Exception:
                self.semver = None

    def __str__(self) -> str:
        return str(self.raw)

    def __bool__(self) -> bool:
        return bool(self.raw) and bool(self.semver)

    def __lt__(self, rhs: "SemVer") -> bool:
        if self.raw is None:
            return rhs.raw is not None
        elif rhs.raw is None:
            return False

        if self.semver is None:
            return rhs.semver is not None
        elif rhs.semver is None:
            return False

        return self.semver < rhs.semver


def _parse_version(tag: str, m: bool | re.Match[str] | None) -> SemVer:
    if not m:
        return SemVer(None)

    if m is True:
        return SemVer(tag)

    return SemVer(m.group("version"))


def _match_fn(match: str | None) -> Callable[[str], bool | re.Match[str] | None] | str:
    if not match:
        return bool
    elif match.startswith("/") and match.endswith("/"):
        return re.compile(f"^{match[1:-1]}$").search
    elif "*" in match:
        return partial(fnmatchcase, pat=match)
    else:
        return match


def get_release(gh: GitHub, repo: str, match: str | None) -> tuple[Release, SemVer]:
    o, r = repo.split("/", 1)

    if not match:
        rls = gh.rest.repos.get_latest_release(owner=o, repo=r).parsed_data
        return rls, _parse_version(rls.tag_name, True)

    test = _match_fn(match)

    if isinstance(test, str):
        rls = gh.rest.repos.get_release_by_tag(o, r, test).parsed_data
        return rls, SemVer(rls.tag_name)

    for rls in gh.rest.paginate(gh.rest.repos.list_releases, owner=o, repo=r):
        if rls.prerelease:
            continue
        if m := test(rls.tag_name):
            return rls, _parse_version(rls.tag_name, m)

    raise RuntimeError(f"No matching release in {repo}: {match}")


def get_tag(gh: GitHub, repo: str, match: str | None) -> tuple[str, SemVer]:
    o, r = repo.split("/", 1)
    test = _match_fn(match)

    if isinstance(test, str):
        return test, SemVer(test)

    tag, ver = max(
        [
            (x.name, _parse_version(x.name, test(x.name)))
            for x in gh.rest.paginate(gh.rest.repos.list_tags, owner=o, repo=r)
        ],
        key=lambda x: x[1],
    )

    if ver:
        return tag, ver

    raise RuntimeError(f"No matching tag in {repo}: {match}")
