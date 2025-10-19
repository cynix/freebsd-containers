package version

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

type VersionConfig struct {
	version string
	regex   *regexp.Regexp
	index   int
}

func (v VersionConfig) IsZero() bool {
	return v.version == "" && v.regex == nil
}

func (v VersionConfig) Resolve() (string, error) {
	if strings.HasPrefix(v.version, "https://") {
		return v.get()
	}

	return v.version, nil
}

func (v VersionConfig) get() (string, error) {
	r, err := http.Get(v.version)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()

	if r.StatusCode >= 400 {
		return "", fmt.Errorf("failed to request version: %v", r.Status)
	}

	b, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}

	if v.regex == nil {
		return string(b), nil
	}

	i := v.regex.SubexpIndex("version")
	if i < 0 {
		return "", fmt.Errorf("invalid version regex: %v", v.regex)
	}

	m := v.regex.FindStringSubmatch(string(b))
	if len(m) <= i {
		return "", fmt.Errorf("%q did not match version regex: %v", v.version, v.regex)
	}

	return m[i], nil
}

func (v *VersionConfig) UnmarshalYAML(b []byte) error {
	var s string

	if yaml.Unmarshal(b, &s) == nil {
		v.version = s
		return nil
	}

	var raw struct {
		URL   string
		Regex string
	}

	if yaml.UnmarshalWithOptions(b, &raw, yaml.DisallowUnknownField()) == nil {
		if raw.URL == "" || !strings.HasPrefix(raw.URL, "https://") {
			return fmt.Errorf("invalid url in version config: %q", raw.URL)
		}

		v.version = raw.URL

		if raw.Regex != "" {
			regex, err := regexp.Compile(raw.Regex)
			if err != nil {
				return err
			}

			index := regex.SubexpIndex("version")
			if index < 0 {
				return fmt.Errorf("invalid version config regex: %q", raw.Regex)
			}

			v.regex = regex
			v.index = index
		}

		return nil
	}

	return fmt.Errorf("invalid version config")
}
