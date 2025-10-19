package utils

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Firecracker struct {
	Self       string
	Addr       string
	User       string
	PrivateKey string

	c *ssh.Client
	s *ssh.Session
	w io.WriteCloser
	r io.Reader

	m string
	b *bufio.Reader
}

func (fc *Firecracker) Close() {
	if fc.s != nil {
		fc.send(cmdShutdown, nil)

		fc.s.Wait()
		fc.s.Close()
		fc.s = nil
	}

	if fc.c != nil {
		fc.c.Close()
		fc.c = nil
	}
}

func (fc *Firecracker) Command(name string, args ...string) *Cmd {
	return Command(name, args...).Via(fc)
}

func (fc *Firecracker) Run(cmd *exec.Cmd) error {
	if fc.w == nil {
		if err := fc.init(); err != nil {
			return err
		}
	}

	if err := fc.send(cmdExec, execute{Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir}); err != nil {
		return err
	}

	if cmd.Stdin != nil {
		if _, err := io.Copy(fc.w, cmd.Stdin); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(fc.w, "\n"); err != nil {
			return err
		}
	}

	if err := fc.send(cmdEOF, nil); err != nil {
		return err
	}

	for {
		m, err := fc.next()
		if err != nil {
			return err
		}

		switch m.Command {
		case "":
			if cmd.Stdout != nil {
				if _, err := cmd.Stdout.Write([]byte(m.Data)); err != nil {
					return err
				}
			}

		case cmdExited:
			if m.Data != "" && m.Data != "\n" {
				var e string
				if err := json.Unmarshal([]byte(m.Data), &e); err != nil {
					return fmt.Errorf("could not parse exit status %q: %w", m.Data, err)
				}

				return fmt.Errorf("command exited with error: %s", e)
			}

			return nil

		default:
			return fmt.Errorf("unexpected message from remote: %q", m.Command)
		}
	}
}

func (fc *Firecracker) init() error {
	if fc.Self == "" {
		return fmt.Errorf("missing remote executable")
	}

	if fc.Addr == "" {
		fc.Addr = "172.16.0.2:22"
	}
	if fc.User == "" {
		fc.User = "root"
	}
	if fc.PrivateKey == "" {
		fc.PrivateKey = "/etc/ssh/freebsd.id_rsa"
	}

	key, err := os.ReadFile(fc.PrivateKey)
	if err != nil {
		return fmt.Errorf("could not read private key %q: %w", fc.PrivateKey, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("could not parse private key %q: %w", fc.PrivateKey, err)
	}

	if fc.c, err = ssh.Dial("tcp", fc.Addr, &ssh.ClientConfig{
		User:            fc.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}); err != nil {
		return fmt.Errorf("could not ssh to %q: %w", fc.Addr, err)
	}

	exe, err := os.Open(fc.Self)
	if err != nil {
		return fmt.Errorf("could not read executable %q: %w", fc.Self, err)
	}
	defer exe.Close()

	var sf *sftp.Client
	if sf, err = sftp.NewClient(fc.c); err != nil {
		return fmt.Errorf("could not sftp to %q: %w", fc.Addr, err)
	}
	defer sf.Close()

	dst := "/tmp/build-" + rand.Text()

	up, err := sf.Create(dst)
	if err != nil {
		return fmt.Errorf("could not create executable on %q: %w", fc.Addr, err)
	}
	defer up.Close()

	if _, err := io.Copy(up, exe); err != nil {
		return fmt.Errorf("could not upload executable on %q: %w", fc.Addr, err)
	}

	if err = sf.Chmod(dst, 0o700); err != nil {
		return fmt.Errorf("could not chmod executable on %q: %w", fc.Addr, err)
	}

	if fc.s, err = fc.c.NewSession(); err != nil {
		return fmt.Errorf("could not open session on %q: %w", fc.Addr, err)
	}

	fc.s.Stderr = os.Stderr

	if fc.w, err = fc.s.StdinPipe(); err != nil {
		return fmt.Errorf("could not connect stdin for %q: %w", fc.Addr, err)
	}

	if fc.r, err = fc.s.StdoutPipe(); err != nil {
		return fmt.Errorf("could not connect stdout for %q: %w", fc.Addr, err)
	}

	fc.m = rand.Text()
	fc.b = bufio.NewReader(fc.r)

	if err = fc.s.Start(fmt.Sprintf("env GITHUB_EVENT_PATH=/dev/null GITHUB_TOKEN=x %s serve %s", dst, fc.m)); err != nil {
		return fmt.Errorf("could not run self on %q: %w", fc.Addr, err)
	}

	return nil
}

func (fc *Firecracker) send(command string, data any) error {
	if _, err := fmt.Fprintf(fc.w, "%s:%s:", fc.m, command); err != nil {
		return err
	}

	if data != nil {
		if err := json.NewEncoder(fc.w).Encode(data); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(fc.w, "\n"); err != nil {
		return err
	}

	return nil
}

func (fc *Firecracker) next() (m message, err error) {
	var line string

	if line, err = fc.b.ReadString('\n'); err != nil {
		return
	}

	if !strings.HasPrefix(line, fc.m) {
		m.Data = line
		return
	}

	m.Command, m.Data, _ = strings.Cut(line[len(fc.m)+1:len(line)-1], ":")
	return
}

func (fc *Firecracker) serve() error {
	var cmd *exec.Cmd
	var stdin io.WriteCloser
	var stdout io.ReadCloser

	for {
		m, err := fc.next()
		if err != nil {
			return err
		}

		switch m.Command {
		case cmdExec:
			if cmd != nil {
				return fmt.Errorf("duplicate exec")
			}

			var ex execute
			if err = json.Unmarshal([]byte(m.Data), &ex); err != nil {
				return fmt.Errorf("could not parse exec command: %w", err)
			}

			cmd = exec.Command(ex.Args[0], ex.Args[1:]...)

			if len(ex.Env) > 0 {
				cmd.Env = append(os.Environ(), ex.Env...)
			}

			cmd.Dir = ex.Dir
			cmd.Stderr = os.Stderr

			if stdin, err = cmd.StdinPipe(); err != nil {
				return fmt.Errorf("could not connect stdin: %w", err)
			}

			if stdout, err = cmd.StdoutPipe(); err != nil {
				return fmt.Errorf("could not connect stdout: %w", err)
			}

			fmt.Fprintf(os.Stderr, "[FC] %q (%q)\n", cmd.Args, cmd.Dir)

			if err = cmd.Start(); err != nil {
				if err = fc.send(cmdExited, err.Error()); err != nil {
					return fmt.Errorf("could not send exit status: %w", err)
				}
			}

		case "":
			if stdin == nil {
				continue
			}

			if _, err := stdin.Write([]byte(m.Data)); err != nil {
				return fmt.Errorf("could not write stdin: %w", err)
			}

		case cmdEOF:
			if cmd == nil || stdin == nil {
				return fmt.Errorf("unexpected EOF from remote")
			}

			stdin.Close()
			stdin = nil

			b := bufio.NewReader(stdout)

			for {
				line, err := b.ReadString('\n')
				if err != nil {
					break
				}

				if _, err = fc.w.Write([]byte(line)); err != nil {
					return fmt.Errorf("could not forward stdout: %w", err)
				}
			}

			if err = cmd.Wait(); err != nil {
				err = fc.send(cmdExited, err.Error())
			} else {
				err = fc.send(cmdExited, nil)
			}
			if err != nil {
				return fmt.Errorf("could not send exit status: %w", err)
			}

			cmd = nil

		case cmdShutdown:
			return exec.Command("halt", "-p").Run()

		default:
			return fmt.Errorf("unexpected message from remote: %q", m.Command)
		}
	}
}

func ServeFirecracker(marker string, r io.Reader, w io.Writer) error {
	if marker == "" {
		return fmt.Errorf("invalid message marker")
	}

	fc := &Firecracker{
		w: os.Stdout,
		r: os.Stdin,
		m: marker,
		b: bufio.NewReader(os.Stdin),
	}
	return fc.serve()
}

type message struct {
	Command string
	Data    string
}

type execute struct {
	Args []string
	Env  []string
	Dir  string
}

const (
	cmdExec     = "exec"
	cmdEOF      = "eof"
	cmdExited   = "exited"
	cmdShutdown = "shutdown"
)
