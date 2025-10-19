package project

import (
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/google/go-github/v74/github"
)

type PackageJob struct {
	Package string `json:"package"`
	Builder string `json:"builder"`
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
}

type ProjectJob struct {
	Project    string       `json:"project"`
	Version    string       `json:"version"`
	Packages   []PackageJob `json:"packages"`
	Containers []string     `json:"containers"`
}

type Project interface {
	Hydrate(name string)
	Job(gh *github.Client) (ProjectJob, error)
	BuildPackage(core utils.Core, gh *github.Client, version, name string) error
	BuildContainer(core utils.Core, gh *github.Client, version, name string) error
}

type BaseProject struct {
	Name string
	Arch []string
}
