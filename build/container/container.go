package container

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"strings"

	"github.com/bobg/go-generics/v4/slices"
	"github.com/cynix/freebsd-binaries/build/project"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/goccy/go-yaml"
	"github.com/google/go-github/v74/github"
)

type ContainerProject struct {
	project.BaseProject `yaml:",inline"`
	Container           ContainerConfig
}

type ContainerConfig struct {
	Base       string
	Assets     []Asset
	Env        map[string]string
	User       string
	Script     string
	Entrypoint containerEntrypoint
}

type containerEntrypoint []string

func (cp *ContainerProject) Hydrate(name string) {
	cp.Name = name

	if len(cp.Arch) == 0 {
		cp.Arch = []string{"amd64", "arm64"}
	}

	for i := range cp.Container.Assets {
		a := &cp.Container.Assets[i]
		switch x := a.Deployable.(type) {
		case *ArchiveAsset:
			if len(x.Files) == 0 {
				x.Files = append(x.Files, ArchiveFile{Src: "{package}"})
			}

			for i := range x.Files {
				f := &x.Files[i]
				if f.Dst == "" {
					f.Dst = "/usr/local/bin/"
				}
			}

		case *FileAsset:
			if x.Dst == "" {
				x.Dst = "/usr/local/{package}/"
			}

		case *ReleaseAsset:
			if len(x.Files) == 0 {
				x.Files = append(x.Files, ArchiveFile{Src: "{package}"})
			}

			for i := range x.Files {
				f := &x.Files[i]
				if f.Dst == "" {
					f.Dst = "/usr/local/bin/"
				}
			}
		}
	}
}

func (cp *ContainerProject) Job(gh *github.Client) (project.ProjectJob, error) {
	return project.ProjectJob{
		Project:    cp.Name,
		Containers: []string{cp.Name},
	}, nil
}

func (cp *ContainerProject) BuildPackage(core utils.Core, gh *github.Client, version, name string) error {
	return fmt.Errorf("no such package to build: %q", name)
}

func (cp *ContainerProject) BuildContainer(core utils.Core, gh *github.Client, version, name string) error {
	return cp.Container.Build(core, gh, containerInfo{Project: cp.Name, Version: version, Package: name}, cp.Arch)
}

func (conf *ContainerConfig) Hydrate(defaults ContainerConfig) {
	if conf.Base == "" {
		conf.Base = defaults.Base
	}

	if len(conf.Assets) == 0 {
		conf.Assets = slices.Clone(defaults.Assets)
	}

	if len(conf.Env) == 0 {
		maps.Copy(conf.Env, defaults.Env)
	}

	if conf.User == "" {
		conf.User = defaults.User
	}

	if conf.Script == "" {
		conf.Script = defaults.Script
	}

	if len(conf.Entrypoint) == 0 {
		conf.Entrypoint = slices.Clone(defaults.Entrypoint)
	}
}

func (conf ContainerConfig) Build(core utils.Core, gh *github.Client, ci containerInfo, archs []string) error {
	if len(archs) == 0 {
		return fmt.Errorf("no arch defined")
	}

	if len(conf.Assets) == 0 {
		return fmt.Errorf("no assets defined")
	}

	fc, err := utils.NewFirecracker("build/build.freebsd_amd64", "172.16.0.2:22", "root", "/etc/ssh/freebsd.id_rsa")
	if err != nil {
		return fmt.Errorf("could not connect to FreeBSD VM: %w", err)
	}
	defer fc.Close()

	setup, err := os.Open("build/setup-freebsd.sh")
	if err != nil {
		return fmt.Errorf("could not read setup-freebsd.sh: %w", err)
	}
	defer setup.Close()

	if err := core.Group("Setting up FreeBSD", func() error { return fc.Command("sh", "-e").WithInput(setup).Run() }); err != nil {
		return fmt.Errorf("could not run setup-freebsd.sh: %w", err)
	}

	base := conf.base()
	if err := core.Group(fmt.Sprintf("Pulling %s", base), func() error { return fc.Command("podman", "pull", base).Run() }); err != nil {
		return fmt.Errorf("could not pull %q: %w", base, err)
	}

	if ci.FreeBSD, err = fc.Command("podman", "image", "inspect", "--format={{index .Annotations \"org.freebsd.version\"}}", base).First(); err != nil {
		return fmt.Errorf("could not inspect %q: %w", base, err)
	}

	latest := fmt.Sprintf("ghcr.io/cynix/%s:latest", ci.Package)
	var tagged string

	if ci.Version != "" {
		tagged = fmt.Sprintf("ghcr.io/cynix/%s:%s", ci.Package, ci.Version)
	}

	for _, ci.Arch = range archs {
		if tagged, err = conf.build(core, gh, fc, "/mnt/firecracker", ci, latest, tagged, base); err != nil {
			return err
		}
	}

	return core.Group("Pushing images", func() error {
		if err := fc.Command("buildah", "login", "--username="+os.Getenv("GITHUB_ACTOR"), "--password="+os.Getenv("GITHUB_TOKEN"), "ghcr.io").Run(); err != nil {
			return fmt.Errorf("could not login to ghcr.io: %w", err)
		}

		if err := fc.Command("buildah", "manifest", "push", "--all", latest, "docker://"+latest).Run(); err != nil {
			return fmt.Errorf("could not push %q: %w", latest, err)
		}

		if tagged != "" {
			if err := fc.Command("buildah", "manifest", "push", "--all", latest, "docker://"+tagged).Run(); err != nil {
				return fmt.Errorf("could not push %q: %w", tagged, err)
			}
		}

		return nil
	})
}

func (conf ContainerConfig) base() string {
	if conf.Base != "" {
		return "ghcr.io/cynix/" + conf.Base
	}

	for _, a := range conf.Assets {
		if _, ok := a.Deployable.(PkgAsset); ok {
			return "ghcr.io/cynix/freebsd:runtime"
		}
	}

	return "ghcr.io/cynix/freebsd:static"
}

func (conf ContainerConfig) build(core utils.Core, gh *github.Client, fc *utils.Firecracker, mnt string, ci containerInfo, latest, tagged, base string) (string, error) {
	core.Info("Building arch: %s", ci.Arch)

	switch ci.Arch {
	case "amd64":
		ci.Triple = "x86_64-unknown-freebsd"
	case "arm64":
		ci.Triple = "aarch64-unknown-freebsd"
	default:
		return tagged, fmt.Errorf("unsupported arch: %q", ci.Arch)
	}

	c := &container{l: core, fc: fc, manifest: latest}
	if err := c.Create(base, ci.Arch); err != nil {
		return tagged, fmt.Errorf("could not create %s container: %w", ci.Arch, err)
	}
	defer c.Close()

	if fi, err := os.Stat(path.Join(ci.Package, "root")); err == nil && fi.IsDir() {
		c.l.Info("Copying %s/root", ci.Package)

		if err = utils.CopyDir(path.Join(mnt, c.root), path.Join(ci.Package, "root")); err != nil {
			return tagged, fmt.Errorf("could not copy %s/root: %w", ci.Package, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tagged, fmt.Errorf("could not open %q: %w", path.Join(ci.Package, "root"), err)
	}

	if user, uid, _ := strings.Cut(conf.User, "="); uid != "" {
		if err := c.l.Group(fmt.Sprintf("Creating user %q = %s", user, uid), func() error {
			if err := fc.Command("pw", "-R", c.root, "groupadd", "-n", user, "-g", uid).Run(); err != nil {
				return fmt.Errorf("could not create group %q: %w", user, err)
			}
			if err := fc.Command("pw", "-R", c.root, "useradd", "-n", user, "-u", uid, "-g", user, "-d", "/nonexistent", "-s", "/sbin/nologin").Run(); err != nil {
				return fmt.Errorf("could not create user %q: %w", user, err)
			}
			return nil
		}); err != nil {
			return tagged, err
		}
	}

	args := []string{"--cmd=[]"}

	for _, a := range conf.Assets {
		ai, err := a.Deploy(core, gh, fc, mnt, c.root, ci)
		if err != nil {
			return tagged, err
		}

		if len(conf.Entrypoint) == 0 && ai.InferredEntrypoint != "" {
			c.l.Info("Deduced entrypoint: %q", ai.InferredEntrypoint)
			conf.Entrypoint = []string{ai.InferredEntrypoint}

			if _, ok := a.Deployable.(FileAsset); ok {
				if err := os.Chmod(path.Join(mnt, c.root, ai.InferredEntrypoint), 0o755); err != nil {
					return tagged, fmt.Errorf("could not chmod entrypoint %q: %w", ai.InferredEntrypoint, err)
				}
			}
		}

		if tagged == "" && ai.InferredVersion != "" {
			c.l.Info("Deduced image version: %q", ai.InferredVersion)
			tagged = fmt.Sprintf("ghcr.io/cynix/%s:%s", ci.Package, ai.InferredVersion)
		}

		for k, v := range ai.Annotations {
			args = append(args, fmt.Sprintf("--annotation=%s=%s", k, v))
		}
	}

	if err := os.Chmod(path.Join(mnt, c.root, "/usr/local/sbin"), 0o711); err != nil && !errors.Is(err, os.ErrNotExist) {
		return tagged, fmt.Errorf("could not chmod /usr/local/sbin: %w", err)
	}

	if conf.Script != "" {
		if err := c.l.Group("Running build script", func() error {
			return fc.Command("sh", "-ex").In(c.root).WithInput(conf.Script).Run()
		}); err != nil {
			return tagged, fmt.Errorf("could not run build script: %w", err)
		}
	}

	entrypoint := strings.Join(slices.Map(conf.Entrypoint, func(s string) string {
		return fmt.Sprintf("%q", s)
	}), ",")

	args = append(args, fmt.Sprintf("--entrypoint=[%s]", entrypoint), "--cmd=")

	for k, v := range conf.Env {
		args = append(args, fmt.Sprintf("--env=%s=%s", k, v))
	}

	if user, _, _ := strings.Cut(conf.User, "="); user != "" {
		args = append(args, fmt.Sprintf("--user=%s:%s", user, user))
	}

	if err := c.l.Group("Configuring image", func() error {
		for _, arg := range args {
			c.l.Info("%s", arg)
		}
		return c.Buildah("config", args...).Run()
	}); err != nil {
		return tagged, fmt.Errorf("could not configure %s container: %w", ci.Arch, err)
	}

	if err := c.l.Group("Committing image", func() error {
		return c.Commit()
	}); err != nil {
		return tagged, fmt.Errorf("could not commit %s container: %w", ci.Arch, err)
	}

	return tagged, nil
}

func (ep *containerEntrypoint) UnmarshalYAML(b []byte) error {
	var s string

	if yaml.Unmarshal(b, &s) == nil {
		*ep = []string{s}
		return nil
	}

	var a []string

	if yaml.Unmarshal(b, &a) == nil {
		*ep = a
		return nil
	}

	return fmt.Errorf("entrypoint should be a string or a list of strings")
}

type container struct {
	l        utils.Core
	fc       *utils.Firecracker
	manifest string
	id       string
	root     string
}

func (c *container) Create(base, arch string) error {
	return c.l.Group(fmt.Sprintf("Creating %s image from %s", arch, base), func() (err error) {
		if c.id, err = c.fc.Command("buildah", "from", "--arch="+arch, base).First(); err != nil {
			return
		}

		if c.root, err = c.Buildah("mount").First(); err != nil {
			c.Buildah("rm")
			return
		}

		return
	})
}

func (c *container) Close() {
	if c.root != "" {
		c.Buildah("unmount")
	}

	if c.id != "" {
		c.Buildah("rm")
	}
}

func (c *container) Commit() (err error) {
	if err = c.Buildah("unmount").Run(); err != nil {
		return
	}
	c.root = ""

	if err = c.Buildah("commit", "--manifest="+c.manifest, "--rm").Run(); err != nil {
		return
	}
	c.id = ""

	return
}

func (c *container) Buildah(command string, args ...string) *utils.Cmd {
	return utils.Command("buildah", slices.Concat([]string{command}, args, []string{c.id})...).Via(c.fc)
}
