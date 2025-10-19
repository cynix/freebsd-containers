package packages

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"

	"github.com/actions-go/toolkit/core"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/bobg/go-generics/v4/slices"
	"github.com/cynix/freebsd-binaries/build/container"
	"github.com/cynix/freebsd-binaries/build/project"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/google/go-github/v74/github"
	"github.com/mholt/archives"
)

type CargoProject struct {
	PackageProject `yaml:",inline"`
	Packages       map[string]CargoPackage
	Defaults       struct {
		Package   CargoConfig
		Container ContainerConfig
	}
}

type CargoPackage struct {
	CargoConfig `yaml:",inline"`
	Binaries    []string
	Container   *ContainerConfig
}

type CargoConfig struct {
	RustConfig `yaml:",inline"`
	Files      []string
}

func (cp *CargoProject) Hydrate(name string) {
	cp.Name = name

	if len(cp.Arch) == 0 {
		cp.Arch = []string{"amd64", "arm64"}
	}

	if len(cp.Packages) == 0 {
		cp.Packages = map[string]CargoPackage{name: {}}
	}

	for k, v := range cp.Packages {
		if len(v.Binaries) == 0 {
			v.Binaries = []string{k}
		}

		v.Hydrate(cp.Defaults.Package)

		if v.Container != nil {
			v.Container.Hydrate(cp.Defaults.Container.ContainerConfig)

			if len(v.Container.Files) == 0 {
				v.Container.Files = cp.Defaults.Container.Files
			}

			v.Container.Assets = slices.Insert(v.Container.Assets, 0, container.Asset{
				Deployable: container.ArchiveAsset{
					URLAsset: container.URLAsset{
						URL: "https://github.com/cynix/freebsd-binaries/releases/download/{project}-v{version}/{package}-v{version}-{triple}.tar.gz",
					},
					Files: v.Container.Files,
				},
			})
		}

		cp.Packages[k] = v
	}
}

func (cp *CargoProject) Job(gh *github.Client) (j project.ProjectJob, err error) {
	j.Project = cp.Name

	var ref string
	if ref, j.Version, err = cp.Source.RefVersion(gh); err != nil {
		return
	}

	for _, k := range slices.Sorted(maps.Keys(cp.Packages)) {
		j.Packages = append(j.Packages, project.PackageJob{Package: k, Builder: cp.Builder, Repo: cp.Source.Repo, Ref: ref})

		if cp.Packages[k].Container != nil {
			j.Containers = append(j.Containers, k)
		}
	}

	return
}

func (cp *CargoProject) BuildPackage(gh *github.Client, version, name string) error {
	pkg, ok := cp.Packages[name]
	if !ok {
		return fmt.Errorf("unknown package: %q", name)
	}

	if err := cp.ApplyPatches(); err != nil {
		return err
	}

	return pkg.Build(name, version, cp.Arch)
}

func (cp *CargoProject) BuildContainer(gh *github.Client, version, name string) error {
	pkg, ok := cp.Packages[name]
	if !ok {
		return fmt.Errorf("unknown package: %q", name)
	}

	if pkg.Container == nil {
		return fmt.Errorf("not building container for package: %q", name)
	}

	c := container.ContainerProject{
		BaseProject: cp.BaseProject,
		Container:   pkg.Container.ContainerConfig,
	}

	return c.BuildContainer(gh, version, name)
}

func (cp *CargoPackage) Build(name, version string, arch []string) error {
	for _, a := range arch {
		if err := cp.build(name, version, a); err != nil {
			return err
		}
	}

	return nil
}

func (cp *CargoPackage) build(name, version, arch string) (err error) {
	var triple string

	switch arch {
	case "amd64":
		triple = "x86_64-unknown-freebsd"
	case "arm64":
		triple = "aarch64-unknown-freebsd"
	default:
		err = fmt.Errorf("unsupported arch: %q", arch)
		return
	}

	dx := utils.Dockcross{Arch: arch}

	args := []string{
		"build",
		"--target=" + triple,
		"--profile=" + cp.Profile,
		"--manifest-path=" + cp.Manifest,
		fmt.Sprintf("--config=profile.%s.strip=\"symbols\"", cp.Profile),
	}

	if len(cp.Features) > 0 {
		for _, feature := range cp.Features {
			if feature == "-default" {
				args = append(args, "--no-default-features")
			} else {
				args = append(args, "--features="+feature)
			}
		}
	}

	if arch == "arm64" {
		args = append(args, "-Z", "build-std=core,std,alloc,proc_macro,panic_abort")
	}

	if core.Group("Building "+cp.Manifest, func() {
		err = dx.Command("cargo", args...).In("src").Run()
	}); err != nil {
		err = fmt.Errorf("could not build %s package: %w", arch, err)
		return
	}

	tarball := fmt.Sprintf("%s-v%s-%s.tar.gz", name, version, triple)
	if core.Group("Creating "+tarball, func() {
		var root *os.Root
		if root, err = os.OpenRoot("src"); err != nil {
			return
		}

		var files []archives.FileInfo

		for _, bin := range cp.Binaries {
			fi := archives.FileInfo{
				NameInArchive: bin,
				Open:          func() (fs.File, error) { return root.Open(bin) },
			}
			bin = path.Join("target", triple, cp.Profile, bin)

			core.Infof("Adding %q as %q", bin, fi.NameInArchive)

			if fi.FileInfo, err = root.Stat(bin); err != nil {
				return
			}

			if fi.Mode().Perm()&0o111 != 0o111 {
				err = fmt.Errorf("not an executable: %q", bin)
				return
			}

			files = append(files, fi)
		}

		for _, glob := range cp.Files {
			core.Infof("Globbing %q", glob)

			var found []string
			if found, err = doublestar.Glob(root.FS(), glob, doublestar.WithFailOnIOErrors(), doublestar.WithFilesOnly()); err != nil {
				err = fmt.Errorf("could not glob %q: %w", glob, err)
				return
			}

			for _, file := range found {
				core.Infof("Adding %q", file)

				fi := archives.FileInfo{
					NameInArchive: file,
					Open:          func() (fs.File, error) { return root.Open(file) },
				}

				if fi.FileInfo, err = root.Stat(file); err != nil {
					return
				}

				files = append(files, fi)
			}
		}

		if err = os.MkdirAll("dist", 0o755); err != nil {
			err = fmt.Errorf("could not create dist dir: %w", err)
			return
		}

		var f *os.File
		if f, err = os.Create(path.Join("dist", tarball)); err != nil {
			return
		}
		defer f.Close()

		format := archives.CompressedArchive{
			Compression: archives.Gz{CompressionLevel: 2},
			Archival:    archives.Tar{NumericUIDGID: true, Uid: 0, Gid: 0},
		}

		if err = format.Archive(context.TODO(), f, files); err != nil {
			return
		}
	}); err != nil {
		return
	}

	return
}

func (c *CargoConfig) Hydrate(defaults CargoConfig) {
	if c.Manifest == "" {
		if c.Manifest = defaults.Manifest; c.Manifest == "" {
			c.Manifest = "Cargo.toml"
		}
	}

	if c.Profile == "" {
		if c.Profile = defaults.Profile; c.Profile == "" {
			c.Profile = "release"
		}
	}

	if len(c.Features) == 0 {
		c.Features = slices.Clone(defaults.Features)
	}

	if len(c.Files) == 0 {
		if c.Files = slices.Clone(defaults.Files); len(c.Files) == 0 {
			c.Files = []string{"COPYING*", "LICENSE*"}
		}
	}
}
