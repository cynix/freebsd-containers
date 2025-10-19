package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/actions-go/toolkit/github"
	"github.com/cynix/freebsd-binaries/build/config"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/goccy/go-yaml"
	"github.com/sanity-io/litter"
)

func main() {
	os.Exit(run(utils.GitHubCore{}))
}

func run(core utils.Core) int {
	if len(os.Args) < 2 {
		core.Fail("Missing subcommand")
		return 1
	}

	if os.Args[1] == "serve" {
		if len(os.Args) < 3 {
			fmt.Println("Missing command marker")
			return 1
		}

		if err := utils.ServeFirecracker(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Printf("Could not serve: %v\n", err)
			return 1
		}

		return 0
	}

	f, err := os.Open("projects.yaml")
	if err != nil {
		core.Fail("Failed to read config: %v", err)
		return 1
	}

	var conf config.Config

	if err := yaml.NewDecoder(f, yaml.DisallowUnknownField()).Decode(&conf); err != nil {
		core.Fail("Failed to parse config: %v", err)
		return 1
	}

	switch os.Args[1] {
	case "dump":
		if len(os.Args) < 3 {
			litter.Dump(conf)
			return 0
		}

		prj, ok := conf.Projects[os.Args[2]]
		if !ok {
			fmt.Println("Invalid project:", prj)
			return 1
		}

		litter.Dump(prj)
		return 0

	case "matrix":
		projects := core.GetInput("projects")
		force := core.GetBoolInput("force")

		switch projects {
		case "":
			core.Fail("No projects specified")
			return 1
		case "all":
			projects = ""
		}

		matrix, err := conf.Matrix(github.GitHub, strings.Split(projects, ","), force)
		if err != nil {
			core.Fail("Failed to generate matrix: %v", err)
			return 1
		}

		core.Group("Generated matrix", func() error {
			litter.Dump(matrix.Include)
			return nil
		})

		b, err := json.Marshal(matrix)
		if err != nil {
			core.Fail("Failed to marshal matrix: %v", err)
			return 1
		}

		core.SetOutput("matrix", string(b))
		return 0

	case "package":
		project := core.GetInput("project")
		version := core.GetInput("version")
		name := core.GetInput("package")

		if project == "" || version == "" || name == "" {
			core.Fail("Missing inputs: project=%q version=%q package=%q", project, version, name)
			return 1
		}

		prj, ok := conf.Projects[project]
		if !ok {
			core.Fail("Unknown project: %q", project)
			return 1
		}

		if err := prj.BuildPackage(core, github.GitHub, version, name); err != nil {
			core.Fail("Failed to build %q: %v", name, err)
			return 1
		}

	case "container":
		project := core.GetInput("project")
		version := core.GetInput("version")
		name := core.GetInput("container")

		if project == "" || name == "" {
			core.Fail("Missing inputs: project=%q version=%q container=%q", project, version, name)
			return 1
		}

		prj, ok := conf.Projects[project]
		if !ok {
			core.Fail("Unknown project: %q", project)
			return 1
		}

		if err := prj.BuildContainer(core, github.GitHub, version, name); err != nil {
			core.Fail("Failed to build %q: %v", name, err)
			return 1
		}

	default:
		fmt.Printf("Invalid subcommand: %q", os.Args[1])
		return 1
	}

	return 0
}
