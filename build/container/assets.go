package container

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cynix/freebsd-binaries/build/utils"
	"github.com/cynix/freebsd-binaries/build/version"
	"github.com/goccy/go-yaml"
	"github.com/google/go-github/v74/github"
	"github.com/mholt/archives"
)

type URLAsset struct {
	URL     string
	Version version.VersionConfig
}

type ArchiveFile struct {
	Src string
	Dst string
}

type ArchiveAsset struct {
	URLAsset
	Files []ArchiveFile
}

type FileAsset struct {
	URLAsset
	Dst string
}

type PkgDeployer struct {
	Pkgs []string

	done map[string]struct{}
}

type PkgAsset struct {
	Name string `yaml:"pkg"`

	deployer *PkgDeployer
}

type ReleaseAsset struct {
	Release version.ReleaseRef
	Glob    string
	Files   []ArchiveFile
}

type containerInfo struct {
	Project string
	Version string
	Package string

	FreeBSD string
	Arch    string
	Triple  string
}

type assetInfo struct {
	InferredVersion    string
	InferredEntrypoint string
	Annotations        map[string]string
}

type Deployable interface {
	Deploy(core utils.Core, gh *github.Client, r utils.Runner, mnt, root string, info containerInfo) (assetInfo, error)
}

type Asset struct {
	Deployable
}

func (ua URLAsset) do(core utils.Core, info containerInfo, v func() (string, error), f func(filename, version string, r io.Reader) error) error {
	if info.Version == "" {
		ver, err := v()
		if err != nil {
			return err
		}

		info.Version = ver
	}

	u := info.Apply(ua.URL)
	uu, err := url.Parse(u)
	if err != nil {
		return err
	}

	return core.Group(fmt.Sprintf("Deploying %q", u), func() error {
		r, err := http.Get(u)
		if err != nil {
			return err
		}
		defer r.Body.Close()

		if r.StatusCode >= 400 {
			return fmt.Errorf("could not download %q: %v", u, r.Status)
		}

		return f(path.Base(uu.Path), info.Version, r.Body)
	})
}

func (aa ArchiveAsset) Deploy(core utils.Core, gh *github.Client, r utils.Runner, mnt, root string, info containerInfo) (ai assetInfo, err error) {
	if len(aa.Files) == 0 {
		err = fmt.Errorf("no files specified for %q", aa.URL)
		return
	}

	err = aa.do(core, info, aa.Version.Resolve, func(filename, version string, body io.Reader) error {
		format, stream, err := archives.Identify(context.TODO(), filename, body)
		if err != nil {
			return err
		}

		ex, ok := format.(archives.Extractor)
		if !ok {
			return fmt.Errorf("could not extract %q", filename)
		}

		ai.InferredVersion = version

		matched := make(map[string]string)
		dirs := make(map[string]string)

		return ex.Extract(context.TODO(), stream, func(ctx context.Context, fi archives.FileInfo) error {
			name := path.Clean(fi.NameInArchive)

			if name == "." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
				core.Warning("Ignoring unsafe path in %q: %q", filename, name)
				return nil
			}

			var dst string
			match := -1

			for src := name; src != "."; src = path.Dir(src) {
				if d, ok := dirs[src]; ok {
					dst = path.Join(d, name[len(src)+1:])
					break
				}
			}

			isDir := fi.IsDir()

			if dst == "" {
				for i, af := range aa.Files {
					af.Src = info.Apply(af.Src)
					af.Dst = info.Apply(af.Dst)

					if af.Src == "" || af.Dst == "" {
						core.Warning("Skipping invalid archive file: %q -> %q", af.Src, af.Dst)
						continue
					}

					if !strings.HasSuffix(af.Src, "/") {
						if isDir {
							continue
						}

						if !doublestar.MatchUnvalidated(af.Src, name) {
							continue
						}

						if existing, ok := matched[af.Src]; ok {
							core.Warning("Ignoring duplicate matches in %q: %q -> %q, %q", filename, af.Src, existing, name)
							return nil
						} else {
							matched[af.Src] = name
						}

						dst = calculateDst(name, info.Apply(af.Dst))
						match = i
						break
					}

					var src string

					for src = name; src != "."; src = path.Dir(src) {
						if doublestar.MatchUnvalidated(af.Src, src) {
							break
						}
					}

					if src == "." {
						continue
					}

					if existing, ok := matched[af.Src]; ok {
						if existing != src {
							core.Warning("Ignoring duplicate matches in %q: %q -> %q, %q", filename, af.Src, existing, src)
							return nil
						}
					} else {
						matched[af.Src] = src
					}

					dst = calculateDst(src, info.Apply(af.Dst))

					if src != name {
						dst = path.Join(dst, name[len(src)+1:])
					}

					break
				}
			}

			if dst == "" {
				core.Debug("Skipping %q", name)

				if isDir {
					return fs.SkipDir
				}
				return nil
			}

			if !isDir && match == 0 && ai.InferredEntrypoint == "" && fi.Mode().Perm()&0o111 == 0o111 {
				ai.InferredEntrypoint = dst
			}

			dst = path.Join(mnt, root, dst)
			core.Info("Extracting %q -> %q", name, dst)

			if err := os.MkdirAll(path.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("could not create dir %q: %w", path.Dir(dst), err)
			}

			if isDir {
				if err := os.Mkdir(dst, fi.Mode().Perm()); err != nil {
					return err
				}

				return nil
			}

			if fi.LinkTarget != "" {
				target := path.Clean(fi.LinkTarget)

				if !strings.HasPrefix(path.Clean(path.Join(root, target)), root) {
					return fmt.Errorf("insecure symlink %q: %q", name, target)
				}

				if err := os.Symlink(target, dst); err != nil {
					return fmt.Errorf("could not create symlink %q -> %q: %w", dst, target, err)
				}

				return nil
			}

			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode())
			if err != nil {
				return fmt.Errorf("could not create file %q: %w", dst, err)
			}
			defer f.Close()

			r, err := fi.Open()
			if err != nil {
				return fmt.Errorf("could not open archive file %q: %w", fi.NameInArchive, err)
			}
			defer r.Close()

			if n, err := io.Copy(f, r); err != nil || n != fi.Size() {
				if err == nil {
					err = fmt.Errorf("wrote %d bytes instead of %d", n, fi.Size())
				}
				return fmt.Errorf("could not copy archive file %q: %w", fi.NameInArchive, err)
			}

			return nil
		})
	})

	return
}

func (aa *ArchiveAsset) UnmarshalYAML(b []byte) error {
	var raw struct {
		Archive string
		Version version.VersionConfig
		Files   []ArchiveFile
	}

	if err := yaml.UnmarshalWithOptions(b, &raw, yaml.DisallowUnknownField()); err != nil {
		return err
	}

	aa.URLAsset = URLAsset{raw.Archive, raw.Version}
	aa.Files = raw.Files

	return nil
}

func (fa FileAsset) Deploy(core utils.Core, gh *github.Client, r utils.Runner, mnt, root string, info containerInfo) (ai assetInfo, err error) {
	err = fa.do(core, info, fa.Version.Resolve, func(filename, version string, body io.Reader) error {
		if fa.Dst == "" {
			fa.Dst = "/usr/local/{package}/"
		}

		dst := calculateDst(filename, info.Apply(fa.Dst))

		ai.InferredEntrypoint = dst
		ai.InferredVersion = version

		dst = path.Join(mnt, root, dst)
		core.Info("Downloading %q -> %q", filename, dst)

		if err := os.MkdirAll(path.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("could not create dir %q: %w", path.Dir(dst), err)
		}

		f, err := os.Create(dst)
		if err != nil {
			return fmt.Errorf("could not create file %q: %w", dst, err)
		}
		defer f.Close()

		if _, err := io.Copy(f, body); err != nil {
			return fmt.Errorf("could not write file %q: %w", dst, err)
		}

		return nil
	})

	return
}

func (fa *FileAsset) UnmarshalYAML(b []byte) error {
	var raw struct {
		File    string
		Version version.VersionConfig
	}

	if err := yaml.UnmarshalWithOptions(b, &raw, yaml.DisallowUnknownField()); err != nil {
		return err
	}

	fa.URLAsset = URLAsset{raw.File, raw.Version}

	return nil
}

func (pd *PkgDeployer) Deploy(core utils.Core, r utils.Runner, mnt, root string, info containerInfo) (ai assetInfo, err error) {
	if _, ok := pd.done[info.Arch]; ok {
		return
	}
	pd.done[info.Arch] = struct{}{}

	freebsd, _, _ := strings.Cut(info.FreeBSD, "p")
	major, minor, ok := strings.Cut(freebsd, ".")
	if !ok || len(major) != 2 || len(minor) != 1 {
		err = fmt.Errorf("invalid FreeBSD version: %q", info.FreeBSD)
		return
	}

	machine := info.Arch
	if machine == "arm64" {
		machine = "aarch64"
	}

	rep := strings.NewReplacer("{major}", major, "{minor}", minor, "{machine}", machine)
	abi := rep.Replace("FreeBSD:{major}:{machine}")
	osv := rep.Replace("{major}0{minor}000")

	if err = core.Group("Installing packages", func() error {
		return pd.pkg(r, abi, osv, root, "install", pd.Pkgs...).Run()
	}); err != nil {
		err = fmt.Errorf("could not install packages: %w", err)
		return
	}

	ai.InferredEntrypoint = "/usr/local/bin/" + pd.Pkgs[0]

	if err = pd.pkg(r, abi, osv, root, "query", append([]string{"%v"}, pd.Pkgs...)...).Each(func(i int, line string) bool {
		if ai.InferredVersion == "" {
			ai.InferredVersion = line
		}

		ai.AddAnnotation(fmt.Sprintf("org.freebsd.pkg.%s.version", pd.Pkgs[i]), line)
		return true
	}); err != nil {
		err = fmt.Errorf("could not query package versions: %w", err)
		return
	}

	if err2 := os.RemoveAll(path.Join(mnt, root, "/var/cache/pkg")); err2 != nil {
		core.Warning("Could not clean up /var/cache/pkg: %v", err2)
	}
	if err2 := os.RemoveAll(path.Join(mnt, root, "/var/db/pkg")); err2 != nil {
		core.Warning("Could not clean up /var/db/pkg: %v", err2)
	}

	hints := map[string]struct{}{"/lib": {}, "/usr/lib": {}, "/usr/local/lib": {}}
	files, _ := filepath.Glob(path.Join(mnt, root, "/usr/local/libdata/ldconfig/*"))

	for _, file := range files {
		var b []byte

		if b, err = os.ReadFile(file); err != nil {
			err = fmt.Errorf("could not read file %q: %w", file, err)
			return
		}

		for line := range strings.Lines(string(b)) {
			if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			hints[line] = struct{}{}
		}
	}

	var dirs []string
	// Since we run `ldconfig` on the host to modify the container,
	// the given dirs must exist on the host too.
	for dir := range hints {
		if err = os.MkdirAll(path.Join(mnt, dir), 0o755); err != nil {
			return
		}

		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)

	if err = r.Command("ldconfig", append([]string{"-f", path.Join(root, "/var/run/ld-elf.so.hints")}, dirs...)...).Run(); err != nil {
		return
	}

	return
}

func (pd *PkgDeployer) pkg(r utils.Runner, abi, osv, root, command string, args ...string) *utils.Cmd {
	return r.Command("pkg", append([]string{"--rootdir", root, command}, args...)...).
		WithEnv("ABI="+abi, "ASSUME_ALWAYS_YES=yes", "OSVERSION="+osv, "PKG_CACHEDIR=/tmp/pkg")
}

func (pa PkgAsset) Deploy(core utils.Core, gh *github.Client, r utils.Runner, mnt, root string, info containerInfo) (assetInfo, error) {
	return pa.deployer.Deploy(core, r, mnt, root, info)
}

func (ra ReleaseAsset) Deploy(core utils.Core, gh *github.Client, r utils.Runner, mnt, root string, info containerInfo) (ai assetInfo, err error) {
	var rls *github.RepositoryRelease
	var ver string

	core.Info("Fetching release: %q", ra.Release.Repo)

	if rls, ver, err = ra.Release.ReleaseVersion(gh); err != nil {
		return
	}

	info.Version = ver
	glob := info.Apply(ra.Glob)

	for _, a := range rls.Assets {
		if ok, _ := path.Match(glob, *a.Name); !ok {
			continue
		}

		aa := ArchiveAsset{
			URLAsset: URLAsset{
				URL: *a.BrowserDownloadURL,
			},
			Files: ra.Files,
		}

		ai, err = aa.Deploy(core, gh, r, mnt, root, info)
		ai.InferredVersion = ver

		if owner, repo, _ := strings.Cut(ra.Release.Repo, "/"); owner != "" && repo != "" {
			ai.AddAnnotation(fmt.Sprintf("com.github.repos.%s.%s.version", owner, repo), ver)
		}

		return
	}

	err = fmt.Errorf("could not find matching asset from release in %q: %q", ra.Release.Repo, glob)
	return
}

func (ci containerInfo) Apply(s string) string {
	return strings.NewReplacer(
		"{project}", ci.Project,
		"{version}", ci.Version,
		"{package}", ci.Package,
		"{arch}", ci.Arch,
		"{triple}", ci.Triple,
	).Replace(s)
}

func (ca *Asset) UnmarshalYAML(b []byte) error {
	pd := &PkgDeployer{done: make(map[string]struct{})}

	var m map[string]any

	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}

	if _, ok := m["pkg"]; ok {
		if err := try[PkgAsset](b, &ca.Deployable); err != nil {
			return err
		}

		pa := ca.Deployable.(*PkgAsset)
		pd.Pkgs = append(pd.Pkgs, pa.Name)
		pa.deployer = pd

		return nil
	}

	if _, ok := m["archive"]; ok {
		return try[ArchiveAsset](b, &ca.Deployable)
	}

	if _, ok := m["file"]; ok {
		return try[FileAsset](b, &ca.Deployable)
	}

	if _, ok := m["release"]; ok {
		return try[ReleaseAsset](b, &ca.Deployable)
	}

	return fmt.Errorf("could not determine asset type")
}

func (ai *assetInfo) AddAnnotation(name, value string) {
	if ai.Annotations == nil {
		ai.Annotations = make(map[string]string)
	}

	ai.Annotations[name] = value
}

func calculateDst(src, dst string) string {
	if !path.IsAbs(dst) {
		panic(fmt.Errorf("dst is not absolute: %q", dst))
	}

	if strings.HasSuffix(dst, "/") {
		dst = dst + path.Base(src)
	}

	return dst
}

func try[T any, D interface {
	*T
	Deployable
}](b []byte, d *Deployable) error {
	t := D(new(T))

	if err := yaml.UnmarshalWithOptions(b, t, yaml.DisallowUnknownField()); err != nil {
		return err
	}

	*d = t
	return nil
}
