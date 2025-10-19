package version

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/goccy/go-yaml"
	"github.com/google/go-github/v74/github"
)

type RefType int

const (
	RefNone RefType = iota
	RefRelease
	RefTag
	RefCommit
)

type RepoRef struct {
	Repo    string
	Type    RefType
	Ref     string
	Version VersionConfig

	regex *regexp.Regexp
	index int
}

type ReleaseRef struct {
	RepoRef `yaml:",inline"`
}

func NewRepoRef(repo, ref string, version VersionConfig) (rr RepoRef, err error) {
	if len(repo) < 3 || strings.Count(repo, "/") != 1 {
		err = fmt.Errorf("invalid repo: %q", repo)
		return
	}

	rr.Repo = repo

	if strings.HasPrefix(ref, "release:") {
		rr.Type = RefRelease
		ref = strings.TrimPrefix(ref, "release:")
	}

	if strings.HasPrefix(ref, "commit:") {
		if rr.Type == RefRelease {
			err = fmt.Errorf("invalid release ref for %q: %q", rr.Repo, ref)
			return
		}

		if ref = strings.TrimPrefix(ref, "commit:"); !commitRegex.MatchString(ref) {
			err = fmt.Errorf("invalid commit ref for %q: %q", rr.Repo, ref)
			return
		}

		rr.Type = RefCommit
	} else if ref == "tag" || strings.HasPrefix(ref, "tag:") {
		if rr.Type == RefRelease {
			err = fmt.Errorf("invalid release ref for %q: %q", rr.Repo, ref)
			return
		}

		rr.Type = RefTag

		if ref == "tag" {
			ref = ""
		} else {
			ref = strings.TrimPrefix(ref, "tag:")
		}
	}

	rr.Ref = ref

	if strings.HasSuffix(rr.Ref, "/") && len(rr.Ref) > 2 {
		ref, regex, ok := strings.Cut(strings.TrimSuffix(rr.Ref, "/"), "/")
		if !ok {
			err = fmt.Errorf("invalid ref regex for %q: %q", rr.Repo, rr.Ref)
			return
		}

		if rr.regex, err = regexp.Compile(regex); err != nil {
			return
		}

		if rr.index = rr.regex.SubexpIndex("version"); rr.index < 0 {
			err = fmt.Errorf("invalid ref regex for %q: %q", rr.Repo, rr.Ref)
			return
		}

		rr.Ref = ref
	}

	if version.IsZero() == (rr.Type == RefCommit) {
		err = fmt.Errorf("inconsistent ref type and version config for %q: %q", rr.Repo, rr.Ref)
		return
	}

	if rr.Type == RefNone {
		rr.Type = RefRelease
	}

	return
}

func (rr RepoRef) RefVersion(gh *github.Client) (string, string, error) {
	if rr.Type == RefNone {
		panic(fmt.Errorf("empty ref"))
	}

	if rr.Type == RefCommit {
		ver, err := rr.Version.Resolve()
		if err != nil {
			return "", "", err
		}

		return rr.Ref, ver, nil
	}

	if rr.Ref != "" && rr.Ref != "@" {
		var ver string

		if rr.regex != nil {
			if m := rr.regex.FindStringSubmatch(rr.Ref); len(m) > rr.index {
				ver = m[rr.index]
			}
		} else {
			ver = strings.TrimPrefix(rr.Ref, "v")
		}

		if _, err := semver.NewVersion(ver); err != nil {
			return "", "", err
		}

		return rr.Ref, ver, nil
	}

	if rr.Type == RefRelease {
		rls, ver, err := rr.ReleaseVersion(gh)
		if err != nil {
			return "", "", err
		}

		if ver == "" {
			return "", "", fmt.Errorf("could not determine version from tag in %q: %q", rr.Repo, *rls.TagName)
		}

		return *rls.TagName, ver, nil
	}

	owner, repo, ok := strings.Cut(rr.Repo, "/")
	if !ok {
		panic(fmt.Errorf("invalid repo: %q", rr.Repo))
	}

	type tagVersion struct {
		Tag     string
		Version string
		sv      *semver.Version
	}
	var found []tagVersion

	tags, _, err := gh.Repositories.ListTags(context.TODO(), owner, repo, &github.ListOptions{PerPage: 100})
	if err != nil {
		return "", "", err
	}

	for _, tag := range tags {
		tv := tagVersion{Tag: *tag.Name}

		if rr.regex != nil {
			m := rr.regex.FindStringSubmatch(tv.Tag)
			if len(m) <= rr.index {
				continue
			}

			tv.Version = m[rr.index]
		} else {
			tv.Version = strings.TrimPrefix(tv.Tag, "v")
		}

		var err error
		if tv.sv, err = semver.StrictNewVersion(tv.Version); err != nil {
			continue
		}

		found = append(found, tv)
	}

	if len(found) == 0 {
		return "", "", fmt.Errorf("no matching tag found in %q: %q", rr.Repo, rr.regex)
	}

	if len(found) > 1 {
		slices.SortFunc(found, func(a, b tagVersion) int {
			// Descending
			return b.sv.Compare(a.sv)
		})
	}

	return found[0].Tag, found[0].Version, nil
}

func (rr RepoRef) ReleaseVersion(gh *github.Client) (rls *github.RepositoryRelease, ver string, err error) {
	if rr.Type != RefRelease {
		err = fmt.Errorf("not a release ref for %q: %q", rr.Repo, rr.Ref)
		return
	}

	owner, repo, ok := strings.Cut(rr.Repo, "/")
	if !ok {
		panic(fmt.Errorf("invalid repo: %q", rr.Repo))
	}

	if (rr.Ref == "" && rr.regex == nil) || rr.Ref == "@" {
		rls, _, err = gh.Repositories.GetLatestRelease(context.TODO(), owner, repo)
	} else if rr.Ref != "" {
		rls, _, err = gh.Repositories.GetReleaseByTag(context.TODO(), owner, repo, rr.Ref)
	}

	if err != nil {
		return
	}

	if rls != nil {
		if rr.regex != nil {
			if m := rr.regex.FindStringSubmatch(*rls.TagName); len(m) > rr.index {
				ver = m[rr.index]
			}
		} else {
			ver = strings.TrimPrefix(*rls.TagName, "v")
		}

		if ver != "" {
			if _, err2 := semver.StrictNewVersion(ver); err2 != nil {
				ver = ""
			}
		}

		return
	}

	if rr.regex == nil {
		panic(fmt.Errorf("invalid release ref for %q", rr.Repo))
	}

	var releases []*github.RepositoryRelease

	if releases, _, err = gh.Repositories.ListReleases(context.TODO(), owner, repo, &github.ListOptions{PerPage: 100}); err != nil {
		return
	}

	type releaseVersion struct {
		Release *github.RepositoryRelease
		Version string
		sv      *semver.Version
	}
	var found []releaseVersion

	for _, rls = range releases {
		if *rls.Prerelease {
			continue
		}

		m := rr.regex.FindStringSubmatch(*rls.TagName)
		if len(m) <= rr.index {
			continue
		}

		rv := releaseVersion{Release: rls, Version: m[rr.index]}

		var err2 error
		if rv.sv, err2 = semver.StrictNewVersion(rv.Version); err2 != nil {
			continue
		}

		found = append(found, rv)
	}

	if len(found) == 0 {
		err = fmt.Errorf("no matching release found in %q: %q", rr.Repo, rr.regex)
		return
	}

	if len(found) > 1 {
		slices.SortFunc(found, func(a, b releaseVersion) int {
			// Descending
			return b.sv.Compare(a.sv)
		})
	}

	return found[0].Release, found[0].Version, nil
}

func (rr *RepoRef) UnmarshalYAML(b []byte) (err error) {
	var raw struct {
		Repo    string
		Ref     string
		Version VersionConfig
	}

	if err = yaml.Unmarshal(b, &raw.Repo); err != nil {
		if err = yaml.UnmarshalWithOptions(b, &raw, yaml.DisallowUnknownField()); err != nil {
			return
		}
	}

	if raw.Repo == "" && raw.Ref == "" {
		err = fmt.Errorf("empty repo ref")
		return
	}

	if *rr, err = NewRepoRef(raw.Repo, raw.Ref, raw.Version); err != nil {
		return
	}

	return
}

func (rr *ReleaseRef) UnmarshalYAML(b []byte) (err error) {
	var raw struct {
		Repo    string
		Ref     string
		Version VersionConfig
	}

	if err = yaml.Unmarshal(b, &raw.Repo); err != nil {
		if err = yaml.UnmarshalWithOptions(b, &raw, yaml.DisallowUnknownField()); err != nil {
			return
		}
	}

	if rr.RepoRef, err = NewRepoRef(raw.Repo, "release:"+raw.Ref, raw.Version); err != nil {
		return
	}

	if rr.Type != RefRelease {
		err = fmt.Errorf("not a release ref for %q: %q", rr.Repo, rr.Ref)
		return
	}

	return
}

var (
	commitRegex = regexp.MustCompile("^[0-9a-f]{1,40}$")
)
