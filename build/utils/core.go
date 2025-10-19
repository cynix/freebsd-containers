package utils

import (
	"crypto/rand"

	"github.com/actions-go/toolkit/core"
)

type Core interface {
	GetInput(name string) string
	GetBoolInput(name string) bool
	SetOutput(name, value string)

	Debug(format string, args ...any)
	Info(format string, args ...any)
	Warning(format string, args ...any)
	Error(format string, args ...any)
	Fail(format string, args ...any)

	Group(string, func() error) error
	Guard(func() error) error
}

type GitHubCore struct{}

func (GitHubCore) GetInput(name string) string {
	v, _ := core.GetInput(name)
	return v
}

func (GitHubCore) GetBoolInput(name string) bool {
	return core.GetBoolInput(name)
}

func (GitHubCore) SetOutput(name, value string) {
	core.SetOutput(name, value)
}

func (GitHubCore) Debug(format string, args ...any) {
	core.Debugf(format, args...)
}

func (GitHubCore) Info(format string, args ...any) {
	core.Infof(format, args...)
}

func (GitHubCore) Warning(format string, args ...any) {
	core.Warningf(format, args...)
}

func (GitHubCore) Error(format string, args ...any) {
	core.Errorf(format, args...)
}

func (GitHubCore) Fail(format string, args ...any) {
	core.SetFailedf(format, args...)
}

func (GitHubCore) Group(name string, fn func() error) error {
	core.StartGroup(name)
	defer core.EndGroup()
	return fn()
}

func (GitHubCore) Guard(fn func() error) error {
	token := rand.Text()
	core.StopCommands(token)
	defer core.StartCommands(token)
	return fn()
}
