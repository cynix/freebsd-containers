package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/actions-go/toolkit/core"
	"github.com/actions-go/toolkit/github"
	"github.com/cynix/freebsd-binaries/build/config"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/goccy/go-yaml"
	"github.com/sanity-io/litter"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		core.SetFailed("Missing subcommand")
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
		core.SetFailedf("Failed to read config: %v", err)
		return 1
	}

	var conf config.Config

	if err := yaml.NewDecoder(f, yaml.DisallowUnknownField()).Decode(&conf); err != nil {
		core.SetFailedf("Failed to parse config: %v", err)
		return 1
	}

	switch os.Args[1] {
	case "dump":
		litter.Dump(conf)
		return 0

	case "matrix":
		projects, _ := core.GetInput("projects")
		force := core.GetBoolInput("force")

		switch projects {
		case "":
			core.SetFailed("No projects specified")
			return 1
		case "all":
			projects = ""
		}

		matrix, err := conf.Matrix(github.GitHub, strings.Split(projects, ","), force)
		if err != nil {
			core.SetFailedf("Failed to generate matrix: %v", err)
			return 1
		}

		core.Group("Generated matrix", func() {
			litter.Dump(matrix.Include)
		})

		b, err := json.Marshal(matrix)
		if err != nil {
			core.SetFailedf("Failed to marshal matrix: %v", err)
			return 1
		}

		core.SetOutput("matrix", string(b))
		return 0

	case "package":
		project, _ := core.GetInput("project")
		version, _ := core.GetInput("version")
		name, _ := core.GetInput("package")

		if project == "" || version == "" || name == "" {
			core.SetFailedf("Missing inputs: project=%q version=%q package=%q", project, version, name)
			return 1
		}

		prj, ok := conf.Projects[project]
		if !ok {
			core.SetFailedf("Unknown project: %q", project)
			return 1
		}

		if err := prj.BuildPackage(github.GitHub, version, name); err != nil {
			core.SetFailedf("Failed to build %q: %v", name, err)
			return 1
		}

	case "container":
		project, _ := core.GetInput("project")
		version, _ := core.GetInput("version")
		name, _ := core.GetInput("container")

		if project == "" || version == "" || name == "" {
			core.SetFailedf("Missing inputs: project=%q version=%q container=%q", project, version, name)
			return 1
		}

		prj, ok := conf.Projects[project]
		if !ok {
			core.SetFailedf("Unknown project: %q", project)
			return 1
		}

		if err := prj.BuildContainer(github.GitHub, version, name); err != nil {
			core.SetFailedf("Failed to build %q: %v", name, err)
			return 1
		}

	default:
		fmt.Printf("Invalid subcommand: %q", os.Args[1])
		return 1
	}

	return 0
}
