package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bobg/go-generics/v4/slices"
	"github.com/cynix/freebsd-binaries/build/container"
	"github.com/cynix/freebsd-binaries/build/packages"
	"github.com/cynix/freebsd-binaries/build/project"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/enrichman/gh-iter/v74"
	"github.com/goccy/go-yaml"
	"github.com/google/go-github/v74/github"
)

type Config struct {
	Projects map[string]project.Project
}

type Matrix struct {
	Include []MatrixJob `json:"include"`
}

type MatrixJob struct {
	Project    string `json:"project"`
	Version    string `json:"version"`
	Packages   string `json:"packages"`
	Containers string `json:"containers"`
}

func (c *Config) Matrix(gh *github.Client, projects []string, force bool) (m Matrix, err error) {
	projects = slices.Filter(slices.Map(projects, strings.TrimSpace), func(s string) bool { return len(s) > 0 })
	if len(projects) == 0 {
		for k := range c.Projects {
			projects = append(projects, k)
		}
	}
	slices.Sort(projects)

	existing := make(map[string]struct{})

	if !force {
		releases := ghiter.NewFromFn2(gh.Repositories.ListReleases, "cynix", "freebsd-binaries").Opts(&github.ListOptions{PerPage: 100})

		for rls := range releases.All() {
			existing[*rls.TagName] = struct{}{}
		}

		if err = releases.Err(); err != nil {
			err = fmt.Errorf("could not list current releases: %w", err)
			return
		}
	}

	for _, k := range projects {
		p, ok := c.Projects[k]
		if !ok {
			err = fmt.Errorf("unknown project: %q", k)
			return
		}

		var j project.ProjectJob
		if j, err = p.Job(gh); err != nil {
			return
		}

		mj := MatrixJob{Project: j.Project, Version: j.Version}
		var b []byte

		if len(j.Packages) > 0 {
			if _, ok := existing[fmt.Sprintf("%s-v%s", j.Project, j.Version)]; !ok {
				if b, err = json.Marshal(j.Packages); err != nil {
					return
				}
				mj.Packages = string(b)
			}
		}

		if len(j.Containers) > 0 {
			if b, err = json.Marshal(j.Containers); err != nil {
				return
			}
			mj.Containers = string(b)
		}

		m.Include = append(m.Include, mj)
	}

	return
}

func (c *Config) UnmarshalYAML(b []byte) error {
	var projects map[string]configProject

	if err := yaml.UnmarshalWithOptions(b, &projects, yaml.DisallowUnknownField()); err != nil {
		return err
	}

	c.Projects = make(map[string]project.Project)

	for k, v := range projects {
		v.p.Hydrate(k)
		c.Projects[k] = v.p
	}

	return nil
}

type configProject struct {
	p project.Project
}

type dummyProject struct {
	packages.PackageProject `yaml:",inline"`
	Packages                map[string]any
	Defaults                struct {
		Package   any
		Container container.ContainerConfig
	}
}

func (dp *dummyProject) Hydrate(name string) {
	dp.Name = name
}

func (dp *dummyProject) Job(gh *github.Client) (project.ProjectJob, error) {
	return project.ProjectJob{Project: dp.Name}, nil
}

func (dp *dummyProject) BuildPackage(core utils.Core, gh *github.Client, version, name string) error {
	return fmt.Errorf("cannot build dummy project %q version %q package %q", dp.Name, version, name)
}

func (dp *dummyProject) BuildContainer(core utils.Core, gh *github.Client, version, name string) error {
	return fmt.Errorf("cannot build dummy project %q version %q container %q", dp.Name, version, name)
}

func (cp *configProject) UnmarshalYAML(b []byte) error {
	var m map[string]any

	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}

	if builder, ok := m["builder"]; ok {
		switch builder {
		case "cargo":
			return try[packages.CargoProject](b, &cp.p)

		case "go":
			fallthrough
		case "cgo":
			return try[packages.GoProject](b, &cp.p)

		default:
			return try[dummyProject](b, &cp.p)
		}
	}

	if _, ok := m["container"]; ok {
		return try[container.ContainerProject](b, &cp.p)
	}

	return fmt.Errorf("could not determine project type")
}

func try[T any, P interface {
	*T
	project.Project
}](b []byte, p *project.Project) error {
	var t T

	if err := yaml.UnmarshalWithOptions(b, &t, yaml.DisallowUnknownField()); err != nil {
		return err
	}

	*p = P(&t)
	return nil
}
