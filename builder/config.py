import re
from enum import Enum
from fnmatch import fnmatchcase
from pathlib import Path
from typing import Annotated, Any, Self

import requests
import ryaml
from githubkit import GitHub
from pydantic import (
    AliasChoices,
    BaseModel,
    Discriminator,
    Field,
    RootModel,
    Tag,
    TypeAdapter,
    ValidationError,
    model_validator,
)

from .utils import SemVer, get_release, get_tag


def _has_field(v: Any, field: str):
    if isinstance(v, dict):
        return field in v
    return hasattr(v, field)


class Architecture(str, Enum):
    AMD64 = "amd64"
    ARM64 = "arm64"


class URLVersion(BaseModel):
    url: str
    regex: str | None = None

    def __str__(self) -> str:
        body = requests.get(self.url).text
        if not self.regex:
            return body.strip().lstrip("v")
        if m := re.search(self.regex, body):
            return m.group("version")
        return ""


class VersionType(str, Enum):
    LITERAL = "literal"
    URL = "url"

    @staticmethod
    def discriminate(v: Any) -> "VersionType | None":
        if isinstance(v, str):
            return VersionType.LITERAL
        elif _has_field(v, "url"):
            return VersionType.URL
        else:
            return None


Version = Annotated[
    Annotated[str, Tag(VersionType.LITERAL)]
    | Annotated[URLVersion, Tag(VersionType.URL)],
    Discriminator(VersionType.discriminate),
]


class AssetType(str, Enum):
    ARCHIVE = "archive"
    FILE = "file"
    PKG = "pkg"
    RELEASE = "release"

    @staticmethod
    def discriminate(v: Any) -> "AssetType | None":
        for typ in AssetType:
            if _has_field(v, typ):
                return typ
        return None


class _URLAsset(BaseModel):
    url: Annotated[str, Field(validation_alias=AliasChoices("archive", "file", "url"))]
    version: Version | None = None

    def resolve(self, **kw) -> tuple[str, SemVer]:
        ver = SemVer(str(self.version) if self.version else None)
        url = self.url

        if url.startswith("%"):
            url = url.lstrip("%").format(version=ver, **kw)

        return url, ver


class ArchiveFile(BaseModel):
    src: str
    dst: str = "/usr/local/bin/"


class ArchiveAsset(_URLAsset):
    files: list[ArchiveFile] = []


class FileAsset(_URLAsset):
    dst: str | None = None


class PkgAsset(BaseModel):
    pkg: str


class ReleaseAsset(BaseModel):
    repo: Annotated[str, Field(validation_alias=AliasChoices("repo", "release"))]
    ref: str | None = None
    glob: str | None = None
    files: list[ArchiveFile] = []

    def resolve(self, gh: GitHub, **kw) -> tuple[str, SemVer]:
        ref = (
            self.ref[1:].format(**kw)
            if self.ref and self.ref.startswith("%")
            else self.ref
        )
        rls, ver = get_release(gh, self.repo, ref)

        glob = (
            self.glob[1:].format(**kw)
            if self.glob and self.glob.startswith("%")
            else self.glob
        )

        for asset in rls.assets:
            if not glob or fnmatchcase(asset.name, glob):
                return asset.browser_download_url, ver

        raise RuntimeError(f"Unable to find asset in {self.repo} @ {ref}")


Asset = Annotated[
    Annotated[ArchiveAsset, Tag(AssetType.ARCHIVE)]
    | Annotated[FileAsset, Tag(AssetType.FILE)]
    | Annotated[PkgAsset, Tag(AssetType.PKG)]
    | Annotated[ReleaseAsset, Tag(AssetType.RELEASE)],
    Discriminator(AssetType.discriminate),
]


class Container(BaseModel):
    base: str | None = None
    assets: list[Asset] = []
    env: dict[str, str | bool | int | float] = {}
    user: str | None = None
    script: str | None = None
    entrypoint: str | list[str] | None = None


class PackageContainer(Container):
    files: list[ArchiveFile] = []


class RustConfig(BaseModel):
    manifest: str | None = None
    profile: str | None = None
    features: list[str] = []


class CargoConfig(RustConfig):
    files: list[str] = []


class CargoPackage(CargoConfig):
    binaries: list[str] = []
    container: Container | None = None


class GoConfig(BaseModel):
    main: str = "./cmd/{binary}"
    flags: list[str] = []
    ldflags: list[str] = []
    tags: list[str] = []
    before: list[str] = []
    files: list[str] = []


class GoPackage(GoConfig):
    binaries: list[str] = []
    container: Container | None = None


class MaturinConfig(RustConfig):
    pass


class MaturinPackage(MaturinConfig):
    pass


class UvConfig(BaseModel):
    pass


class UvPackage(BaseModel):
    pass


class BuilderType(str, Enum):
    CARGO = "cargo"
    CGO = "cgo"
    GO = "go"
    MATURIN = "maturin"
    UV = "uv"

    @staticmethod
    def discriminate(v: Any) -> str:
        builder = (
            v.get("builder", "") if isinstance(v, dict) else getattr(v, "builder", "")
        )
        return BuilderType.GO if builder == BuilderType.CGO else builder


class _Project(BaseModel):
    arch: list[Architecture] = [x for x in Architecture]


class ContainerProject(_Project):
    container: Container

    def populate(self, project: str) -> None:
        pass


class PackageProject(_Project):
    repo: str
    ref: Annotated[str | None, Field(default=None)]
    version: Annotated[Version | None, Field(default=None)]
    builder: BuilderType
    container: Annotated[Container | None, Field(default=None)]

    @model_validator(mode="after")
    def validate_ref_and_version(self) -> Self:
        if self.ref is not None and self.version is not None:
            raise ValidationError("'ref' and 'version' cannot both be set")
        return self

    def resolve(self, gh: GitHub) -> tuple[str, SemVer]:
        rls, ver = None, None

        if ref := self.ref:
            if ref.startswith("commit:"):
                rls, ver = ref[7:], SemVer(None)
            elif ref == "tag" or ref.startswith("tag:"):
                rls, ver = get_tag(gh, self.repo, ref[4:])

        if not rls:
            rls, ver = get_release(gh, self.repo, ref)
            rls = rls.tag_name

        if not ver and self.version:
            ver = SemVer(str(self.version))

        if not ver:
            raise RuntimeError(f"Unable to determine version for repo: {self.repo}")

        return rls, ver


class CargoProject(PackageProject):
    packages: dict[str, CargoPackage] = {}
    defaults: CargoConfig = CargoConfig()

    def populate(self, project: str) -> None:
        if not self.packages:
            self.packages = {project: CargoPackage()}

        for k, v in self.packages.items():
            if not v.binaries:
                v.binaries = [k]

            v.manifest = v.manifest or self.defaults.manifest or "Cargo.toml"
            v.profile = v.profile or self.defaults.profile or "release"
            v.features = v.features or self.defaults.features
            v.files = v.files or self.defaults.files or ["LICENSE"]

            if v.container:
                v.container.assets.insert(
                    0,
                    FileAsset(
                        url="%https://github.com/cynix/freebsd-binaries/releases/download/{project}-v{version}/{package}-v{version}-{triple}.tar.gz"
                    ),
                )


class GoProject(PackageProject):
    packages: dict[str, GoPackage] = {}
    defaults: GoConfig = GoConfig()

    def populate(self, project: str) -> None:
        if not self.packages:
            self.packages = {project: GoPackage()}

        for k, v in self.packages.items():
            if not v.binaries:
                v.binaries = [k]

            v.main = v.main or self.defaults.main
            v.flags = v.flags or self.defaults.flags
            v.ldflags = v.ldflags or self.defaults.ldflags
            v.tags = v.tags or self.defaults.tags
            v.before = v.before or self.defaults.before
            v.files = v.files or self.defaults.files or ["LICENSE"]

            if v.container:
                v.container.assets.insert(
                    0,
                    FileAsset(
                        url="%https://github.com/cynix/freebsd-binaries/releases/download/{project}-v{version}/{package}-{version}-freebsd_{arch}.tar.gz"
                    ),
                )


class MaturinProject(PackageProject):
    packages: dict[str, MaturinPackage] = {}
    defaults: MaturinConfig = MaturinConfig()

    def populate(self, project: str) -> None:
        if not self.packages:
            self.packages = {project: MaturinPackage()}

        for k, v in self.packages.items():
            v.manifest = v.manifest or self.defaults.manifest or "Cargo.toml"
            v.profile = v.profile or self.defaults.profile or "release"
            v.features = v.features or self.defaults.features


class UvProject(PackageProject):
    packages: dict[str, UvPackage] = {}
    defaults: UvConfig = UvConfig()

    def populate(self, project: str) -> None:
        if not self.packages:
            self.packages = {project: UvPackage()}


Project = Annotated[
    Annotated[ContainerProject, Tag("")]
    | Annotated[CargoProject, Tag(BuilderType.CARGO)]
    | Annotated[GoProject, Tag(BuilderType.GO)]
    | Annotated[MaturinProject, Tag(BuilderType.MATURIN)]
    | Annotated[UvProject, Tag(BuilderType.UV)],
    Discriminator(BuilderType.discriminate),
]


class Config(RootModel):
    root: dict[str, Project]

    @staticmethod
    def from_yaml(file: str | Path) -> "Config":
        with open(file) as f:
            config = TypeAdapter(Config).validate_python(ryaml.load(f))

        for k, v in config.root.items():
            v.populate(k)

        return config
