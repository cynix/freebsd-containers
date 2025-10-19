package packages

import (
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/bobg/go-generics/v4/slices"
	"github.com/cynix/freebsd-binaries/build/container"
	"github.com/cynix/freebsd-binaries/build/project"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/goccy/go-yaml"
	"github.com/google/go-github/v74/github"
)

type GoProject struct {
	PackageProject `yaml:",inline"`
	Packages       map[string]GoPackage
	Defaults       struct {
		Package   GoConfig
		Container ContainerConfig
	}
}

type GoPackage struct {
	GoConfig  `yaml:",inline"`
	Binaries  []string
	Container *ContainerConfig
}

type GoConfig struct {
	Main    string
	Flags   []string
	Ldflags []string
	Tags    []string
	Before  []string
	Files   []string
}

func (gp *GoProject) Hydrate(name string) {
	gp.Name = name

	if len(gp.Arch) == 0 {
		gp.Arch = []string{"amd64", "arm64"}
	}

	if len(gp.Packages) == 0 {
		gp.Packages = map[string]GoPackage{name: {}}
	}

	for k, v := range gp.Packages {
		if len(v.Binaries) == 0 {
			v.Binaries = []string{k}
		}

		v.Hydrate(gp.Defaults.Package)

		if v.Container != nil {
			v.Container.Hydrate(gp.Defaults.Container.ContainerConfig)

			if len(v.Container.Files) == 0 {
				v.Container.Files = gp.Defaults.Container.Files
			}

			aa := container.ArchiveAsset{
				URLAsset: container.URLAsset{
					URL: "https://github.com/cynix/freebsd-binaries/releases/download/{project}-v{version}/{package}-{version}-freebsd_{arch}.tar.gz",
				},
			}

			for _, bin := range v.Binaries {
				aa.Files = append(aa.Files, container.ArchiveFile{
					Src: bin,
					Dst: "/usr/local/bin/",
				})
			}

			aa.Files = append(aa.Files, v.Container.Files...)
			v.Container.Assets = slices.Insert(v.Container.Assets, 0, container.Asset{Deployable: aa})
		}

		gp.Packages[k] = v
	}
}

func (gp *GoProject) Job(gh *github.Client) (j project.ProjectJob, err error) {
	j.Project = gp.Name

	var ref string
	if ref, j.Version, err = gp.Source.RefVersion(gh); err != nil {
		return
	}

	for _, k := range slices.Sorted(maps.Keys(gp.Packages)) {
		j.Packages = append(j.Packages, project.PackageJob{Package: k, Builder: gp.Builder, Repo: gp.Source.Repo, Ref: ref})

		if gp.Packages[k].Container != nil {
			j.Containers = append(j.Containers, k)
		}
	}

	return
}

func (gp *GoProject) BuildPackage(core utils.Core, gh *github.Client, version, name string) error {
	pkg, ok := gp.Packages[name]
	if !ok {
		return fmt.Errorf("unknown package: %q", name)
	}

	if err := gp.ApplyPatches(core); err != nil {
		return err
	}

	return pkg.Build(core, name, gp.Arch, gp.Builder == "cgo")
}

func (gp *GoProject) BuildContainer(core utils.Core, gh *github.Client, version, name string) error {
	pkg, ok := gp.Packages[name]
	if !ok {
		return fmt.Errorf("unknown package: %q", name)
	}

	if pkg.Container == nil {
		return fmt.Errorf("not building container for package: %q", name)
	}

	c := container.ContainerProject{
		BaseProject: gp.BaseProject,
		Container:   pkg.Container.ContainerConfig,
	}

	return c.BuildContainer(core, gh, version, name)
}

func (gp *GoPackage) Build(core utils.Core, name string, arch []string, cgo bool) error {
	gr := goReleaser{
		Version:     2,
		ProjectName: name,
		Dist:        "../dist",
		Archives: []goReleaserArchive{
			{
				Formats:      []string{"tar.gz"},
				NameTemplate: "{{ .ProjectName }}-{{ .Version }}-{{ .Os }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}{{ with .Mips }}_{{ . }}{{ end }}{{ if not (eq .Amd64 \"v1\") }}{{ .Amd64 }}{{ end }}",
				Files:        gp.Files,
			},
		},
	}

	gr.Release.Disable = true
	gr.Before.Hooks = gp.Before

	for _, bin := range gp.Binaries {
		gb := goReleaserBuild{
			Id:      bin,
			Binary:  bin,
			Main:    strings.ReplaceAll(gp.Main, "{binary}", bin),
			Flags:   append(gp.Flags, "-trimpath"),
			Ldflags: append(gp.Ldflags, "-buildid=", "-extldflags=-static", "-s", "-w"),
			Tags:    gp.Tags,
			Targets: slices.Map(arch, func(s string) string { return "freebsd_" + s }),
		}

		if cgo {
			gb.Env = []string{
				"CGO_ENABLED=1",
				"CGO_CFLAGS=--target={{ if eq .Arch \"amd64\" }}x86_64{{ else }}aarch64{{ end }}-unknown-freebsd --sysroot=/freebsd/{{ .Arch }}",
				"CGO_LDFLAGS=--target={{ if eq .Arch \"amd64\" }}x86_64{{ else }}aarch64{{ end }}-unknown-freebsd --sysroot=/freebsd/{{ .Arch }} -fuse-ld=lld",
				"PKG_CONFIG_LIBDIR=/freebsd/{{ .Arch }}/usr/libdata/pkgconfig:/freebsd/{{ .Arch }}/usr/local/libdata/pkgconfig",
				"PKG_CONFIG_PATH=",
				"PKG_CONFIG_SYSROOT_DIR=/freebsd/{{ .Arch }}",
			}
		} else {
			gb.Env = []string{"CGO_ENABLED=0"}
		}

		gr.Builds = append(gr.Builds, gb)
	}

	b, err := yaml.Marshal(gr)
	if err != nil {
		return fmt.Errorf("could not marshal .goreleaser.yaml: %w", err)
	}

	core.Group("Generating .goreleaser.yaml", func() error {
		fmt.Println(string(b))
		return nil
	})

	if err := os.WriteFile(".goreleaser.yaml", b, 0o644); err != nil {
		return fmt.Errorf("could not write .goreleaser.yaml: %w", err)
	}

	cmd := utils.Command("/bin/sh", "-c", "pwd && cd src && goreleaser release --config=../.goreleaser.yaml --clean --skip=validate")

	if cgo {
		cmd.Via(&utils.Dockcross{})
	}

	if err := core.Group("Building package", func() error { return cmd.Run() }); err != nil {
		return fmt.Errorf("goreleaser failed: %w", err)
	}

	return nil
}

func (c *GoConfig) Hydrate(defaults GoConfig) {
	if c.Main == "" {
		if c.Main = defaults.Main; c.Main == "" {
			c.Main = "./cmd/{binary}"
		}
	}

	if len(c.Flags) == 0 {
		c.Flags = slices.Clone(defaults.Flags)
	}

	if len(c.Ldflags) == 0 {
		c.Ldflags = slices.Clone(defaults.Ldflags)
	}

	if len(c.Tags) == 0 {
		c.Tags = slices.Clone(defaults.Tags)
	}

	if len(c.Before) == 0 {
		c.Before = slices.Clone(defaults.Before)
	}

	if len(c.Files) == 0 {
		if c.Files = slices.Clone(defaults.Files); len(c.Files) == 0 {
			c.Files = []string{"COPYING*", "LICENSE*"}
		}
	}
}

type goReleaserArchive struct {
	Formats      []string `yaml:"formats"`
	NameTemplate string   `yaml:"name_template"`
	Files        []string `yaml:"files"`
}

type goReleaserBuild struct {
	Id      string   `yaml:"id"`
	Binary  string   `yaml:"binary"`
	Main    string   `yaml:"main"`
	Flags   []string `yaml:"flags"`
	Ldflags []string `yaml:"ldflags"`
	Tags    []string `yaml:"tags"`
	Targets []string `yaml:"targets"`
	Env     []string `yaml:"env"`
}

type goReleaser struct {
	Version     int                 `yaml:"version"`
	ProjectName string              `yaml:"project_name"`
	Dist        string              `yaml:"dist"`
	Archives    []goReleaserArchive `yaml:"archives"`
	Release     struct {
		Disable bool `yaml:"disable"`
	} `yaml:"release"`
	Before struct {
		Hooks []string `yaml:"hooks"`
	} `yaml:"before"`
	Builds []goReleaserBuild `yaml:"builds"`
}
