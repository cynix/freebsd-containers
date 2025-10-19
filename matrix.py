#!/usr/bin/env -S uv run --no-project --script
# /// script
# requires-python = ">=3.13"
# dependencies = [
#     "actions-python-core",
#     "pygithub",
#     "ruamel.yaml",
# ]
# ///

import json
import re
import sys

from actions import core
from github import Github
from ruamel.yaml import YAML


def get_release(gh: Github, repo: str, regex: str) -> tuple[str, str]:
    r = gh.get_repo(repo)
    tag, version = None, None

    try:
        if regex:
            for rls in r.get_releases():
                if rls.prerelease:
                    continue
                if m := re.search(regex, rls.tag_name):
                    tag = rls.tag_name
                    version = m.group(1)
                    break
            else:
                raise RuntimeError(f"No release matching regex: {regex}")
        else:
            tag = r.get_latest_release().tag_name
            if m := re.search("v?(.+)", tag):
                version = m.group(1)
            else:
                raise RuntimeError(f"Unable to determine version: {tag}")
    except Exception as e:
        core.set_failed(e)
        sys.exit(1)

    return tag, version


def main():
    gh = Github()

    with open("projects.yaml") as f:
        y = YAML().load(f)

    projects = core.get_input("projects") or "all"
    matrix = []

    for name in sorted(
        y.keys()
        if projects == "all"
        else list(set(x.strip() for x in projects.split(",")))
    ):
        config = y[name]

        project = {"project": name}
        packages = None
        containers = None

        if go := config.get("go"):
            tag, project["version"] = get_release(gh, go["repo"], go.get("regex"))

            try:
                gh.get_repo("cynix/freebsd-containers").get_release(
                    f"{name}-v{project['version']}"
                )
            except Exception:
                packages = {
                    "go": [
                        {
                            "repo": go["repo"],
                            "ref": tag,
                            "cgo": go.get("cgo", False),
                            "package": k,
                        }
                        for k in sorted(go["packages"].keys())
                    ]
                }

            containers = [
                k
                for k in sorted(go["packages"].keys())
                if "container" in go["packages"][k]
            ]
        elif "container" in config:
            containers = [name]

        if packages:
            project["packages"] = json.dumps(packages)
        if containers:
            project["containers"] = json.dumps(containers)

        matrix.append(project)

    core.set_output("matrix", {"include": matrix})


if __name__ == "__main__":
    main()
