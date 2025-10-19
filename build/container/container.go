package container

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"strings"

	"github.com/actions-go/toolkit/core"
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
}

func (cp *ContainerProject) Job(gh *github.Client) (project.ProjectJob, error) {
	return project.ProjectJob{
		Project:    cp.Name,
		Containers: []string{cp.Name},
	}, nil
}

func (cp *ContainerProject) BuildPackage(gh *github.Client, version, name string) error {
	return fmt.Errorf("no such package to build: %q", name)
}

func (cp *ContainerProject) BuildContainer(gh *github.Client, version, name string) error {
	return cp.Container.Build(gh, containerInfo{Project: cp.Name, Version: version, Package: name}, cp.Arch)
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

func (conf ContainerConfig) Build(gh *github.Client, ci containerInfo, archs []string) (err error) {
	if len(conf.Assets) == 0 {
		err = fmt.Errorf("no assets defined")
		return
	}

	fc := &utils.Firecracker{
		Self:       "build/build.freebsd_amd64",
		Addr:       "172.16.0.2:22",
		User:       "root",
		PrivateKey: "/etc/ssh/freebsd.id_rsa",
	}
	defer fc.Close()

	var setup *os.File
	if setup, err = os.Open("build/setup-freebsd.sh"); err != nil {
		err = fmt.Errorf("could not read setup-freebsd.sh: %w", err)
		return
	}
	defer setup.Close()

	if err = fc.Command("sh", "-e").WithInput(setup).Run(); err != nil {
		err = fmt.Errorf("could not run setup-freebsd.sh: %w", err)
		return
	}

	base := conf.base()
	if err = fc.Command("podman", "pull", base).Run(); err != nil {
		err = fmt.Errorf("could not pull %q: %w", base, err)
		return
	}

	if ci.FreeBSD, err = fc.Command("podman", "image", "inspect", "--format={{index .Annotations \"org.freebsd.version\"}}", base).First(); err != nil {
		err = fmt.Errorf("could not inspect %q: %w", base, err)
		return
	}

	latest := fmt.Sprintf("ghcr.io/cynix/%s:latest", ci.Package)
	var tagged string

	if ci.Version != "" {
		tagged = fmt.Sprintf("ghcr.io/cynix/%s:%s", ci.Package, ci.Version)
	}

	for _, ci.Arch = range archs {
		if tagged, err = conf.build(gh, fc, "/mnt/firecracker", ci, latest, tagged, base); err != nil {
			return
		}
	}

	if err = fc.Command("buildah", "login", "--username="+os.Getenv("GITHUB_ACTOR"), "--password="+os.Getenv("GITHUB_TOKEN"), "ghcr.io").Run(); err != nil {
		err = fmt.Errorf("could not login to ghcr.io: %w", err)
		return
	}

	if err = fc.Command("buildah", "manifest", "push", "--all", latest, "docker://"+latest).Run(); err != nil {
		err = fmt.Errorf("could not push %q: %w", latest, err)
		return
	}

	if tagged != "" {
		if err = fc.Command("buildah", "manifest", "push", "--all", latest, "docker://"+tagged).Run(); err != nil {
			err = fmt.Errorf("could not push %q: %w", tagged, err)
			return
		}
	}

	return nil
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

func (conf ContainerConfig) build(gh *github.Client, fc *utils.Firecracker, mnt string, ci containerInfo, latest, tagged, base string) (string, error) {
	core.Infof("Building arch: %s", ci.Arch)

	switch ci.Arch {
	case "amd64":
		ci.Triple = "x86_64-unknown-freebsd"
	case "arm64":
		ci.Triple = "aarch64-unknown-freebsd"
	default:
		return tagged, fmt.Errorf("unsupported arch: %q", ci.Arch)
	}

	c := &container{fc: fc, manifest: latest}
	if err := c.Create(base, ci.Arch); err != nil {
		return tagged, fmt.Errorf("could not create %s container: %w", ci.Arch, err)
	}
	defer c.Close()

	if fi, err := os.Stat(path.Join(ci.Package, "root")); err == nil && fi.IsDir() {
		core.Infof("Copying %s/root", ci.Package)

		if err = utils.CopyDir(path.Join(mnt, c.root), path.Join(ci.Package, "root")); err != nil {
			return tagged, fmt.Errorf("could not copy %s/root: %w", ci.Package, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tagged, err
	}

	if user, uid, _ := strings.Cut(conf.User, "="); uid != "" {
		core.Infof("Creating user %q = %s", user, uid)

		if err := fc.Command("pw", "-R", c.root, "groupadd", "-n", user, "-g", uid).Run(); err != nil {
			return tagged, fmt.Errorf("could not create group %q: %w", user, err)
		}
		if err := fc.Command("pw", "-R", c.root, "useradd", "-n", user, "-u", uid, "-g", user, "-d", "/nonexistent", "-s", "/sbin/nologin").Run(); err != nil {
			return tagged, fmt.Errorf("could not create user %q: %w", user, err)
		}
	}

	args := []string{"--cmd=[]"}

	for _, a := range conf.Assets {
		ai, err := a.Deploy(gh, fc, mnt, c.root, ci)
		if err != nil {
			return tagged, err
		}

		if len(conf.Entrypoint) == 0 && ai.InferredEntrypoint != "" {
			core.Infof("Deduced entrypoint: %q", ai.InferredEntrypoint)
			conf.Entrypoint = []string{ai.InferredEntrypoint}

			if _, ok := a.Deployable.(FileAsset); ok {
				if err := os.Chmod(path.Join(mnt, c.root, ai.InferredEntrypoint), 0o755); err != nil {
					return tagged, fmt.Errorf("could not chmod entrypoint %q: %w", ai.InferredEntrypoint, err)
				}
			}
		}

		if tagged == "" && ai.InferredVersion != "" {
			core.Infof("Deduced image version: %q", ai.InferredVersion)
			tagged = fmt.Sprintf("ghcr.io/cynix/%s:%s", ci.Package, ai.InferredVersion)
		}

		args = append(args, ai.Annotations...)
	}

	if err := os.Chmod(path.Join(mnt, c.root, "/usr/local/sbin"), 0o711); err != nil && !errors.Is(err, os.ErrNotExist) {
		return tagged, fmt.Errorf("could not chmod /usr/local/sbin: %w", err)
	}

	if conf.Script != "" {
		var err error

		if core.Group("Running build script", func() {
			err = fc.Command("sh", "-ex").In(c.root).WithInput(conf.Script).Run()
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

	core.Group("Configuring image", func() {
		for _, arg := range args {
			core.Info(arg)
		}
	})

	if err := c.Buildah("config", args...).Run(); err != nil {
		return tagged, fmt.Errorf("could not configure %s container: %w", ci.Arch, err)
	}

	if err := c.Commit(); err != nil {
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
	fc       *utils.Firecracker
	manifest string
	id       string
	root     string
}

func (c *container) Create(base, arch string) (err error) {
	core.Infof("Creating %s image from %s", arch, base)

	if c.id, err = utils.Command("buildah", "from", "--arch="+arch, base).Via(c.fc).First(); err != nil {
		return
	}

	if c.root, err = c.Buildah("mount").First(); err != nil {
		c.Buildah("rm")
		return
	}

	return
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
	core.Info("Committing image")

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
