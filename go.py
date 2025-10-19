#!/usr/bin/env -S uv run --no-project --script
# /// script
# requires-python = ">=3.13"
# dependencies = [
#     "actions-python-core",
#     "ruamel.yaml",
# ]
# ///

import subprocess
from pathlib import Path

from actions import core
from ruamel.yaml import YAML


def main():
    yaml = YAML()
    yaml.width = 4096
    yaml.indent(sequence=4, offset=2)

    project = core.get_input("project")
    package = core.get_input("package")

    print("Generating .goreleaser.yaml")

    with open("projects.yaml") as f:
        config = yaml.load(f)[project]
        go = config["go"]

    goreleaser = {
        "version": 2,
        "project_name": package,
        "dist": "../dist",
        "archives": [
            {
                "formats": ["tar.gz"],
                "name_template": '{{ .ProjectName }}-{{ .Version }}-{{ .Os }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}{{ with .Mips }}_{{ . }}{{ end }}{{ if not (eq .Amd64 "v1") }}{{ .Amd64 }}{{ end }}',
                "files": go.get("files", ["LICENSE"]),
            }
        ],
        "release": {
            "disable": True,
        },
    }

    if b := go.get("before", []):
        goreleaser["before"] = {"hooks": b}

    template = go.get("build", {})

    template["flags"] = template.get("flags", []) + ["-trimpath"]
    template["ldflags"] = template.get("ldflags", []) + [
        "-buildid=",
        "-extldflags=-static",
        "-s",
        "-w",
    ]
    template["targets"] = [
        f"freebsd_{x}" for x in config.get("arch", ["amd64", "arm64"])
    ]

    if "env" not in template:
        template["env"] = []

    if go.get("cgo"):
        template["env"].extend(
            [
                "CGO_ENABLED=1",
                'CGO_CFLAGS=--target={{ if eq .Arch "amd64" }}x86_64{{ else }}aarch64{{ end }}-unknown-freebsd --sysroot=/freebsd/{{ .Arch }}',
                'CGO_LDFLAGS=--target={{ if eq .Arch "amd64" }}x86_64{{ else }}aarch64{{ end }}-unknown-freebsd --sysroot=/freebsd/{{ .Arch }} -fuse-ld=lld',
                "PKG_CONFIG_SYSROOT_DIR=/freebsd/{{ .Arch }}",
                "PKG_CONFIG_PATH=/freebsd/{{ .Arch }}/usr/local/libdata/pkgconfig",
            ]
        )
    else:
        template["env"].append("CGO_ENABLED=0")

    goreleaser["builds"] = []

    for binary in go.get("packages", {}).get(package, {}).get("binaries", [project]):
        build = template | {"id": binary, "binary": binary}
        build["main"] = build.get("main", "./cmd/{binary}").format(binary=binary)
        goreleaser["builds"].append(build)

    with open(".goreleaser.yaml", "w") as f:
        yaml.dump(goreleaser, f)

    subprocess.run(["cat", ".goreleaser.yaml"])
    print()

    for patch in Path(".").glob(f"{project}/*.patch"):
        print(f"Applying {patch}")

        with open(patch) as f:
            subprocess.run(["patch", "-p1"], stdin=f, cwd="src", check=True)

    cmd = [
        "sh",
        "-c",
        "cd src; goreleaser release --config=../.goreleaser.yaml --clean --skip=validate",
    ]

    if go.get("cgo"):
        with open("build.sh", "w") as f:
            subprocess.check_call(
                ["docker", "run", "ghcr.io/cynix/dockcross-freebsd"], stdout=f
            )
        cmd = ["bash", "build.sh"] + cmd

    subprocess.run(cmd, check=True)


if __name__ == "__main__":
    main()
