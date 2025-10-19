import json

from actions import core
from actions.github import get_githubkit

from .config import Config, ContainerProject, PackageProject
from .utils import action, action_group


@action
def matrix():
    root = Config.from_yaml("projects.yaml").root

    projects = core.get_input("projects")
    force = core.get_boolean_input("force")

    gh = get_githubkit()

    releases = (
        []
        if force
        else [
            x.tag_name
            for x in gh.rest.paginate(
                gh.rest.repos.list_releases, owner="cynix", repo="freebsd-binaries"
            )
            if not x.prerelease
        ]
    )
    matrix = []

    if not force:
        with action_group("Current releases"):
            for r in releases:
                core.info(r)

    for name in sorted(
        root.keys()
        if projects == "all"
        else set(x.strip() for x in projects.split(","))
    ):
        project = root[name]

        job = {"project": name}
        packages = []
        containers = []

        if isinstance(project, PackageProject):
            ref, ver = project.resolve(gh)
            assert ver.raw
            job["version"] = ver.raw

            if f"{name}-v{ver}" not in releases:
                packages.extend(
                    {
                        "package": k,
                        "builder": project.builder,
                        "repo": project.repo,
                        "ref": ref,
                    }
                    for k in sorted(project.packages.keys())
                )

            containers = [
                k
                for k, v in sorted(project.packages.items(), key=lambda x: x[0])
                if bool(getattr(v, "container", None))
            ]
        elif isinstance(project, ContainerProject):
            containers = [name]
        else:
            raise ValueError(f"Unknown type for project {name}: {project}")

        if packages:
            job["packages"] = json.dumps(packages)
        if containers:
            job["containers"] = json.dumps(containers)

        matrix.append(job)

    core.set_output("matrix", {"include": matrix})
