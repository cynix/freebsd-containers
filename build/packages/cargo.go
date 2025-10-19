package packages

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"

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

			aa := &container.ArchiveAsset{
				URLAsset: container.URLAsset{
					URL: "https://github.com/cynix/freebsd-binaries/releases/download/{project}-v{version}/{package}-v{version}-{triple}.tar.gz",
				},
			}

			for _, bin := range v.Binaries {
				aa.Files = append(aa.Files, container.ArchiveFile{Src: bin})
			}

			aa.Files = append(aa.Files, v.Container.Files...)
			v.Container.Assets = slices.Insert(v.Container.Assets, 0, container.Asset{Deployable: aa})
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

func (cp *CargoProject) BuildPackage(core utils.Core, gh *github.Client, version, name string) error {
	pkg, ok := cp.Packages[name]
	if !ok {
		return fmt.Errorf("unknown package: %q", name)
	}

	if err := cp.ApplyPatches(core); err != nil {
		return err
	}

	return pkg.Build(core, name, version, cp.Arch)
}

func (cp *CargoProject) BuildContainer(core utils.Core, gh *github.Client, version, name string) error {
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
	// Use package name as container name
	c.Hydrate(name)

	return c.BuildContainer(core, gh, version, name)
}

func (cp *CargoPackage) Build(core utils.Core, name, version string, archs []string) error {
	for _, arch := range archs {
		if err := cp.build(core, name, version, arch); err != nil {
			return err
		}
	}

	return nil
}

func (cp *CargoPackage) build(core utils.Core, name, version, arch string) error {
	var triple string

	switch arch {
	case "amd64":
		triple = "x86_64-unknown-freebsd"
	case "arm64":
		triple = "aarch64-unknown-freebsd"
	default:
		return fmt.Errorf("unsupported arch: %q", arch)
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

	if err := core.Group(fmt.Sprintf("Building %s package", arch), func() error {
		return dx.Command("cargo", args...).In("src").Run()
	}); err != nil {
		return fmt.Errorf("could not build %s package: %w", arch, err)
	}

	tarball := fmt.Sprintf("%s-v%s-%s.tar.gz", name, version, triple)
	if err := core.Group("Creating "+tarball, func() error {
		root, err := os.OpenRoot("src")
		if err != nil {
			return err
		}

		var files []archives.FileInfo

		for _, bin := range cp.Binaries {
			fi := archives.FileInfo{
				NameInArchive: bin,
				Open:          func() (fs.File, error) { return root.Open(bin) },
			}
			bin = path.Join("target", triple, cp.Profile, bin)

			core.Info("Adding %q as %q", bin, fi.NameInArchive)

			if fi.FileInfo, err = root.Stat(bin); err != nil {
				return err
			}
			if fi.Mode().Perm()&0o111 != 0o111 {
				return fmt.Errorf("not an executable: %q", bin)
			}

			files = append(files, fi)
		}

		for _, glob := range cp.Files {
			core.Info("Globbing %q", glob)

			found, err := doublestar.Glob(root.FS(), glob, doublestar.WithFailOnIOErrors(), doublestar.WithFilesOnly())
			if err != nil {
				return fmt.Errorf("could not glob %q: %w", glob, err)
			}

			for _, file := range found {
				core.Info("Adding %q", file)

				fi := archives.FileInfo{
					NameInArchive: file,
					Open:          func() (fs.File, error) { return root.Open(file) },
				}

				if fi.FileInfo, err = root.Stat(file); err != nil {
					return err
				}

				files = append(files, fi)
			}
		}

		if err := os.MkdirAll("dist", 0o755); err != nil {
			return fmt.Errorf("could not create dist dir: %w", err)
		}

		f, err := os.Create(path.Join("dist", tarball))
		if err != nil {
			return err
		}
		defer f.Close()

		format := archives.CompressedArchive{
			Compression: archives.Gz{CompressionLevel: 2},
			Archival:    archives.Tar{NumericUIDGID: true, Uid: 0, Gid: 0},
		}

		return format.Archive(context.TODO(), f, files)
	}); err != nil {
		return fmt.Errorf("could not create %q: %w", tarball, err)
	}

	return nil
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
