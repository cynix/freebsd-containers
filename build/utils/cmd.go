package utils

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Runner interface {
	Command(name string, args ...string) *Cmd
	Run(cmd *exec.Cmd) error
}

type Exec struct{}

type Cmd struct {
	c *exec.Cmd
	r Runner
}

func Command(name string, args ...string) *Cmd {
	c := &Cmd{c: exec.Command(name, args...)}
	c.c.Stderr = os.Stderr
	return c
}

func (c *Cmd) In(cwd string) *Cmd {
	c.c.Dir = cwd
	return c
}

func (c *Cmd) WithEnv(env ...string) *Cmd {
	c.c.Env = append(c.c.Env, env...)
	return c
}

func (c *Cmd) WithInput(input any) *Cmd {
	switch x := input.(type) {
	case io.Reader:
		c.c.Stdin = x
	case []byte:
		c.c.Stdin = bytes.NewReader(x)
	case string:
		c.c.Stdin = strings.NewReader(x)
	}
	return c
}

func (c *Cmd) Via(r Runner) *Cmd {
	c.r = r
	return c
}

func (c *Cmd) Run() error {
	if c.r == nil {
		c.r = Exec{}
	}

	c.c.Stdout = os.Stdout
	return c.r.Run(c.c)
}

func (c *Cmd) Each(yield func(int, string) bool) error {
	if c.r == nil {
		c.r = Exec{}
	}

	var b bytes.Buffer
	c.c.Stdout = &b

	if err := c.r.Run(c.c); err != nil {
		return err
	}

	i := 0

	for line := range strings.Lines(b.String()) {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}

		if !yield(i, line) {
			break
		}

		i++
	}

	return nil
}

func (c *Cmd) First() (string, error) {
	var first string

	err := c.Each(func(_ int, line string) bool {
		first = line
		return false
	})

	return first, err
}

func (Exec) Command(name string, args ...string) *Cmd {
	return Command(name, args...)
}

func (Exec) Run(c *exec.Cmd) error {
	fmt.Fprintf(os.Stderr, "::debug::[GH] %q\n", c.Args)

	if len(c.Env) > 0 {
		c.Env = append(os.Environ(), c.Env...)
	}

	return c.Run()
}
