package shell

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

//const _GOSH_beg = "__@@GOSH@@__{{"
//const _GOSH_end = "}}"
const _GOSH_feed = "echo __@@GOSH@@__{{%d:$?}}"
const _BUFSZ = 2 << 8

var gosh_re = regexp.MustCompile(`(?P<name>(.|\n)*)__@@GOSH@@__{{(\d+?:\d+?)}}`)
var gosh_re2 = regexp.MustCompile(`^__@@GOSH@@__{{(\d+?:\d+?)}}`)

var sh_debug = false

func init() {
	do_debug := os.Getenv("GO_SHELL_DEBUG")
	if do_debug == "1" {
		sh_debug = true
	}
}

func fprintf(w io.Writer, format string, a ...interface{}) (int, error) {
	if sh_debug {
		return fmt.Fprintf(w, format, a...)
	}
	return 0, nil
}

func gosh_feed(i int) string {
	return fmt.Sprintf(_GOSH_feed, i)
}

// Shell holds the context for an interactive shell (/bin/sh)
type Shell struct {
	cmd    *exec.Cmd
	stdin  io.Writer
	stdout io.Reader
	icmd   chan int
	resp   chan response
	quit   chan struct{}
}

type response struct {
	id  int    // command index
	buf []byte // message payload
	err error  // error
}

var g_counter = 0

func (sh *Shell) id() int {
	g_counter += 1
	sh.icmd <- g_counter
	return g_counter
}

// New returns a new interactive /bin/sh shell.
// Note that to properly release resources, you have to call
// the Delete() method once the Shell value is no longer needed.
func New() (Shell, error) {
	cmd := exec.Command("/bin/sh")
	stdin, w := io.Pipe()
	cmd.Stdin = stdin

	r, stdout := io.Pipe()
	cmd.Stdout = stdout
	cmd.Stderr = stdout

	sh := Shell{
		cmd:    cmd,
		stdin:  w,
		stdout: r,
		icmd:   make(chan int),
		resp:   make(chan response),
		quit:   make(chan struct{}),
	}

	go func(sh *Shell) {
		rem := make([]byte, 0)
		for {
			select {
			case <-sh.quit:
				fprintf(os.Stderr, "quit...\n")
				err := sh.cmd.Process.Kill()
				fprintf(os.Stderr, "quit...: %v\n", err)
				return

			case i := <-sh.icmd:
				fprintf(os.Stderr, "............................\n")
				var err error

				buf := make([]byte, len(rem))
				copy(buf, rem)
				buf = append(buf, make([]byte, _BUFSZ)...)
				rem = rem[:0]

				fprintf(os.Stderr, "%d: ==>\n", i)
				fprintf(os.Stderr, "buf: [%q]\n", string(buf))
				fprintf(os.Stderr, "rem: [%q]\n", string(rem))

				n, err := sh.stdout.Read(buf)
				fprintf(os.Stderr, "==> n=%v (err=%v)\n", n, err)
				buf = buf[:n]
				fprintf(os.Stderr, "==> n=%v (err=%v) [%q] [match=%v][%v]\n", n, err, buf, gosh_re.Match(buf), buf)
				for !gosh_re.Match(buf) {
					fprintf(os.Stderr, "..acc..\n")
					tmp := make([]byte, _BUFSZ)
					n, err2 := sh.stdout.Read(tmp)
					buf = append(buf, tmp[:n]...)
					fprintf(os.Stderr, "==> n=%v (err2=%v) [%q] [match=%v][%v]\n", n, err2, buf, gosh_re.Match(buf), buf)
					if err2 != nil {
						err = err2
						break
					}
					fprintf(os.Stderr, "~~> n=%v (err2=%v) [%q] [match=%v][%v]\n", n, err2, buf, gosh_re.Match(buf), buf)
				}
				if err == nil {
					// first test with gosh_re2 to handle the crashing case
					if gosh_re2.Match(buf) {
						fprintf(os.Stderr, "--> re2\n")
						all := gosh_re2.FindSubmatchIndex(buf)
						idx := all[2:]
						// no payload.
						// just command-index + return-code
						circ := buf[idx[0]:idx[1]] // command-index + return code
						rem = append(rem, buf[idx[1]+2:]...)
						fprintf(os.Stderr, "+++ rem=<%q>\n", string(rem))
						buf = []byte("")
						tmp := bytes.Split(circ, []byte(":"))
						//cmd := tmp[0]
						ret := tmp[1]
						if !bytes.Equal(ret, []byte("0")) {
							err = fmt.Errorf("error: %q", ret)
						}

					} else {
						fprintf(os.Stderr, "--> re1\n")
						all := gosh_re.FindSubmatchIndex(buf)
						idx := all[2:]
						msg_idx := idx[:len(idx)-2]
						msg_beg := msg_idx[0]
						msg_end := msg_idx[0]
						for ii := 0; ii < len(msg_idx); ii += 2 {
							fprintf(os.Stderr, "%02d: %q\n", ii, buf[idx[ii]:idx[ii+1]])
							msg_end = msg_idx[ii+1]
						}
						msg := buf[msg_beg:msg_end] // message
						rc_beg := idx[len(idx)-2]
						rc_end := idx[len(idx)-1]
						circ := buf[rc_beg:rc_end] // command-index + return code
						rem = append(rem, buf[idx[3]+2:]...)
						fprintf(os.Stderr, "msg: %q\n", msg)
						fprintf(os.Stderr, "crc: %q\n", circ)
						fprintf(os.Stderr, "+++ rem=<%q>\n", string(rem))
						buf = msg
						tmp := bytes.Split(circ, []byte(":"))
						//cmd := tmp[0]
						ret := tmp[1]
						if !bytes.Equal(ret, []byte("0")) {
							err = fmt.Errorf("error: %q", ret)
						}
					}
				}
				fprintf(os.Stderr, "%d: <<< [%q] (err=%v)\n", i, string(buf), err)
				sh.resp <- response{id: i, buf: buf, err: err}
			}
		}
	}(&sh)
	err := cmd.Start()
	if err != nil {
		return sh, err
	}
	err = sh.Setenv("TERM", "vt100")
	if err != nil {
		return sh, err
	}

	return sh, nil
}

// Setenv sets the value of the environment variable named by the key.
// It returns an error, if any.
func (sh *Shell) Setenv(key, value string) error {
	// fprintf(os.Stderr, ":: env[%q]= %q\n", key, value)
	resp := sh.send(fmt.Sprintf("export %s=%q", key, value))
	return resp.err
}

// Getenv retrieves the value of the environment variable named by the key.
// It returns the value, which will be empty if the variable is not present.
func (sh *Shell) Getenv(key string) string {
	// fprintf(os.Stderr, ":: env[%q]\n", key)
	resp := sh.send(fmt.Sprintf("echo ${%s}", key))
	if resp.err != nil {
		return ""
	}
	out := string(resp.buf)
	out = strings.Trim(out, "\r\n")
	// fprintf(os.Stderr, ":: env[%q] [done]\n", key)
	return out
}

// Source sources the script with optional arguments args.
// It returns the combined output of stdout and stderr, as well as an error if any.
func (sh *Shell) Source(script string, args ...string) ([]byte, error) {
	// fprintf(os.Stderr, ":: source[%q]\n", script)
	resp := sh.send(fmt.Sprintf(". %s %s", script, strings.Join(args, " ")))
	return resp.buf, resp.err
}

// Run runs the command cmd with optional arguments args.
// It returns the combined output of stdout and stderr, as well as an error if any.
func (sh *Shell) Run(cmd string, args ...string) ([]byte, error) {
	shcmd := strings.Join(append([]string{cmd}, args...), " ")
	resp := sh.send(shcmd)
	// fprintf(os.Stderr, ":: run %q... [done]\n", shcmd)
	return resp.buf, resp.err
}

// Delete cleans up resources
func (sh *Shell) Delete() error {
	sh.quit <- struct{}{}
	return nil
}

func (sh *Shell) send(cmd string) response {
	i := sh.id()
	req := fmt.Sprintf("%s\n%s\n", cmd, gosh_feed(i))
	fprintf(os.Stderr, "%03d >>> %q\n", i, req)
	_, err := sh.stdin.Write([]byte(req))
	if err != nil {
		return response{id: i, buf: nil, err: err}
	}
	resp := <-sh.resp
	return resp
}

// Chdir changes the current working directory to the named directory. If
// there is an error, it will be of type *PathError.
func (sh *Shell) Chdir(dir string) error {
	resp := sh.send(fmt.Sprintf("cd %q", dir))
	return resp.err
}

// Clearenv deletes all environment variables.
func (sh *Shell) Clearenv() {
	envs := sh.Environ()
	clear := make([]string, 0, len(envs))
	for _, val := range envs {
		toks := strings.SplitN(val, "=", 2)
		key := toks[0]
		clear = append(
			clear,
			fmt.Sprintf("unset %s;", key),
		)
	}
	resp := sh.send(strings.Join(clear, ""))
	if resp.err != nil {
		return
	}
}

// Environ returns a copy of strings representing the environment, in the
// form "key=value".
func (sh *Shell) Environ() []string {
	resp := sh.send("/usr/bin/printenv")
	if resp.err != nil {
		return nil
	}
	blines := bytes.Split(bytes.Trim(resp.buf, "\n"), []byte("\n"))
	envs := make([]string, 0, len(blines))
	for _, bline := range blines {
		envs = append(envs, string(bline))
	}
	return envs
}

// Getwd returns a rooted path name corresponding to the current directory.
// If the current directory can be reached via multiple paths (due to
// symbolic links), Getwd may return any one of them.
func (sh *Shell) Getwd() (pwd string, err error) {
	resp := sh.send("pwd")
	if resp.err != nil {
		return "", resp.err
	}
	return string(bytes.Trim(resp.buf, "\n")), resp.err
}

// EOF
