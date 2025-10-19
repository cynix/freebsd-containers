package packages

import (
	"os"
	"path/filepath"

	"github.com/actions-go/toolkit/core"
	"github.com/cynix/freebsd-binaries/build/container"
	"github.com/cynix/freebsd-binaries/build/project"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/cynix/freebsd-binaries/build/version"
)

type PackageProject struct {
	project.BaseProject `yaml:",inline"`
	Source              version.RepoRef
	Builder             string
}

type RustConfig struct {
	Manifest string
	Profile  string
	Features []string
}

type ContainerConfig struct {
	container.ContainerConfig `yaml:",inline"`
	Files                     []container.ArchiveFile
}

func (pp *PackageProject) ApplyPatches() error {
	patches, err := filepath.Glob(pp.Name + "/*.patch")
	if err != nil {
		return err
	}

	for _, patch := range patches {
		f, err := os.Open(patch)
		if err != nil {
			return err
		}
		defer f.Close()

		if core.Group("Applying "+patch, func() {
			err = utils.Command("patch", "-p1").In("src").WithInput(f).Run()
		}); err != nil {
			return err
		}
	}

	return nil
}
