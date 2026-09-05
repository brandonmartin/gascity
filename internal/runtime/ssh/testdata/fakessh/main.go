// Command fakessh is a PATH-injected ssh(1) stand-in for SSH provider
// conformance. When GC_SSH_CONFORMANCE_STATE is set it simulates a remote
// tmux box on disk; otherwise it execs the next real ssh on PATH.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	state := os.Getenv("GC_SSH_CONFORMANCE_STATE")
	if state == "" {
		proxyRealSSH()
		return
	}
	os.Exit(runFake(state, os.Args[1:]))
}

func proxyRealSSH() {
	self, _ := os.Executable()
	selfDir := filepath.Dir(self)
	sshPath, err := lookPathSkipDir("ssh", selfDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakessh: real ssh not found: %v\n", err)
		os.Exit(255)
	}
	cmd := exec.Command(sshPath, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		ee := &exec.ExitError{}
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		os.Exit(255)
	}
}

func lookPathSkipDir(name, skipDir string) (string, error) {
	skipDir, _ = filepath.Abs(skipDir)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		abs, _ := filepath.Abs(dir)
		if abs == skipDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: not found", name)
}

func runFake(state string, sshArgv []string) int {
	if err := os.MkdirAll(filepath.Join(state, "sessions"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	remote, err := remoteCommand(sshArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakessh: %v\n", err)
		return 1
	}
	argv, err := parseQuotedArgv(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakessh: parse remote %q: %v\n", remote, err)
		return 1
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "fakessh: empty remote command")
		return 1
	}
	return handleRemote(state, argv)
}

func remoteCommand(sshArgv []string) (string, error) {
	seenDD := false
	seenTarget := false
	var remote string
	for i := 0; i < len(sshArgv); i++ {
		arg := sshArgv[i]
		if !seenDD {
			switch arg {
			case "--":
				seenDD = true
			case "-o", "-i", "-p", "-F", "-l", "-E":
				i++
			}
			continue
		}
		if !seenTarget {
			seenTarget = true
			continue
		}
		remote = arg
	}
	if !seenTarget {
		return "", fmt.Errorf("missing destination")
	}
	return remote, nil
}

func parseQuotedArgv(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var args []string
	i := 0
	for i < len(s) {
		if s[i] == ' ' || s[i] == '\t' {
			i++
			continue
		}
		if s[i] != '\'' {
			return nil, fmt.Errorf("expected quoted arg at %d", i)
		}
		i++
		var b strings.Builder
		for i < len(s) {
			if s[i] == '\'' {
				if i+3 < len(s) && s[i:i+4] == `'\''` {
					b.WriteByte('\'')
					i += 4
					continue
				}
				i++
				break
			}
			b.WriteByte(s[i])
			i++
		}
		args = append(args, b.String())
	}
	return args, nil
}

func handleRemote(state string, argv []string) int {
	switch argv[0] {
	case "tmux":
		return handleTmux(state, argv[1:])
	case "pgrep":
		return 1
	case "sh":
		return handleSh(state, argv[1:])
	case "rm":
		return 0
	default:
		return 0
	}
}

func handleSh(state string, args []string) int {
	if len(args) == 0 {
		dir, err := os.MkdirTemp(state, "gc-session-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(abs)
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	return 0
}

func handleTmux(state string, args []string) int {
	if len(args) == 0 {
		return 1
	}
	switch args[0] {
	case "has-session":
		name := flagValue(args, "-t")
		if sessionExists(state, name) {
			return 0
		}
		return 1
	case "new-session":
		name := flagValue(args, "-s")
		if name == "" {
			return 1
		}
		if err := createSession(state, name); err != nil {
			return 1
		}
		return 0
	case "kill-session":
		name := flagValue(args, "-t")
		if !sessionExists(state, name) {
			return 1
		}
		_ = os.Remove(sessionPath(state, name))
		_ = os.RemoveAll(metaDir(state, name))
		return 0
	case "list-sessions":
		entries, err := os.ReadDir(filepath.Join(state, "sessions"))
		if err != nil {
			return 1
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			return 1
		}
		fmt.Print(strings.Join(names, "\n"))
		return 0
	case "set-environment":
		return handleSetEnv(state, args[1:])
	case "show-environment":
		return handleShowEnv(state, args[1:])
	case "display-message":
		return handleDisplay(state, args[1:])
	case "send-keys", "capture-pane", "clear-history", "respawn-pane", "start-server":
		if args[0] == "start-server" {
			// Secret-env launch path: treat as success. Conformance does not
			// send secrets, so this is a defensive no-op.
			return 0
		}
		if args[0] == "capture-pane" {
			return 0
		}
		return 0
	default:
		return 0
	}
}

func handleSetEnv(state string, args []string) int {
	name := flagValue(args, "-t")
	if name == "" || !sessionExists(state, name) {
		return 1
	}
	unset := false
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			i++
		case "-u":
			unset = true
		default:
			if strings.HasPrefix(args[i], "-") {
				continue
			}
			positional = append(positional, args[i])
		}
	}
	if unset {
		if len(positional) == 0 {
			return 1
		}
		_ = os.Remove(metaPath(state, name, positional[0]))
		return 0
	}
	if len(positional) < 2 {
		return 1
	}
	if err := os.MkdirAll(metaDir(state, name), 0o755); err != nil {
		return 1
	}
	if err := os.WriteFile(metaPath(state, name, positional[0]), []byte(positional[1]), 0o600); err != nil {
		return 1
	}
	return 0
}

func handleShowEnv(state string, args []string) int {
	name := flagValue(args, "-t")
	if name == "" {
		return 1
	}
	var key string
	for i := 0; i < len(args); i++ {
		if args[i] == "-t" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			key = args[i]
		}
	}
	if key == "" {
		return 1
	}
	body, err := os.ReadFile(metaPath(state, name, key))
	if err != nil {
		fmt.Printf("-%s\n", key)
		return 0
	}
	fmt.Printf("%s=%s\n", key, string(body))
	return 0
}

func handleDisplay(state string, args []string) int {
	name := flagValue(args, "-t")
	format := flagValue(args, "-p")
	switch format {
	case "#{session_attached}":
		fmt.Println("0")
		return 0
	case "#{session_activity}":
		if !sessionExists(state, name) {
			return 1
		}
		st, err := os.Stat(sessionPath(state, name))
		if err != nil {
			return 1
		}
		fmt.Println(strconv.FormatInt(st.ModTime().Unix(), 10))
		return 0
	default:
		return 0
	}
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func sessionPath(state, name string) string {
	return filepath.Join(state, "sessions", name)
}

func sessionExists(state, name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(sessionPath(state, name))
	return err == nil
}

func createSession(state, name string) error {
	f, err := os.OpenFile(sessionPath(state, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func metaDir(state, name string) string {
	return filepath.Join(state, "meta", name)
}

func metaPath(state, name, key string) string {
	return filepath.Join(metaDir(state, name), key)
}
