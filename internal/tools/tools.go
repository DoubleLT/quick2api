// Package tools provides the server-side tool executor: it intercepts
// tool_use blocks the upstream model emits and runs the requested tool
// locally so the client gets real effects instead of an advisory
// contract. Only tools explicitly enabled in config are executable; any
// other name the model invents fails fast with an error result.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Executor dispatches a tool_use request to a local implementation.
type Executor interface {
	// Exec runs the named tool with the model-supplied JSON input.
	// Returns the tool_result text and whether the call errored.
	Exec(ctx context.Context, name string, input json.RawMessage) (result string, isErr bool)
}

// Registry is the enabled-tool table loaded from config.
type Registry struct {
	cfg     Config
	enabled map[string]bool
}

// Config mirrors the `tools:` section of config.yaml.
type Config struct {
	// Bash controls shell-command tools ("Bash", "sh", "shell").
	Bash struct {
		Enabled  bool     `yaml:"enabled"`
		Allow    []string `yaml:"allow"` // glob patterns; "*" allows everything
		Cwd      string   `yaml:"cwd"`   // working dir; empty = process cwd
		TimeoutS int      `yaml:"timeout_seconds"`
	} `yaml:"bash"`
	// Files controls the file tools ("Read", "Write", "Edit", "Glob",
	// "Grep", "LS", "MultiEdit", "TodoWrite"). Root confines all paths.
	Files struct {
		Enabled bool   `yaml:"enabled"`
		Root    string `yaml:"root"`
	} `yaml:"files"`
}

// New builds a Registry from config. A disabled section means its tools
// answer "tool not enabled" instead of running.
func New(cfg Config) *Registry {
	r := &Registry{cfg: cfg, enabled: map[string]bool{}}
	if cfg.Bash.Enabled {
		for _, n := range []string{"Bash", "bash", "sh", "shell"} {
			r.enabled[n] = true
		}
	}
	if cfg.Files.Enabled {
		for _, n := range []string{
			"Read", "read", "Write", "write", "Edit", "edit",
			"MultiEdit", "multiedit", "Glob", "glob", "Grep", "grep",
			"LS", "ls", "TodoWrite", "todowrite", "NotebookEdit",
		} {
			r.enabled[n] = true
		}
	}
	return r
}

// Has reports whether a tool name is configured.
func (r *Registry) Has(name string) bool { return r.enabled[name] }

// Exec dispatches to the right section; unknown names return an error
// result so the model sees the failure instead of a fabricated answer.
func (r *Registry) Exec(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	if !r.enabled[name] {
		return fmt.Sprintf("tool %q is not enabled in quick2api (see config tools:)", name), true
	}
	switch {
	case isBash(name):
		return r.execBash(ctx, input)
	case isTodo(name):
		return "todo list acknowledged", false
	case isLS(name):
		return r.execLS(input)
	case isGlob(name):
		return r.execGlob(input)
	case isGrep(name):
		return r.execGrep(input)
	case isRead(name):
		return r.execRead(input)
	case isWrite(name):
		return r.execWrite(input)
	case isEdit(name):
		return r.execEdit(input)
	default:
		return fmt.Sprintf("tool %q has no implementation", name), true
	}
}

func isBash(n string) bool  { return n == "Bash" || n == "bash" || n == "sh" || n == "shell" }
func isTodo(n string) bool  { return n == "TodoWrite" || n == "todowrite" }
func isLS(n string) bool    { return n == "LS" || n == "ls" }
func isGlob(n string) bool  { return n == "Glob" || n == "glob" }
func isGrep(n string) bool  { return n == "Grep" || n == "grep" }
func isRead(n string) bool  { return n == "Read" || n == "read" }
func isWrite(n string) bool { return n == "Write" || n == "write" }
func isEdit(n string) bool {
	return n == "Edit" || n == "edit" || n == "MultiEdit" || n == "multiedit" || n == "NotebookEdit"
}

// ---- bash ----------------------------------------------------------------

var bashArg = struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}{}

func (r *Registry) execBash(ctx context.Context, input json.RawMessage) (string, bool) {
	var in struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fmt.Sprintf("bad Bash input: %v", err), true
	}
	if in.Command == "" {
		return "Bash requires a non-empty command", true
	}
	if !bashAllowed(in.Command, r.cfg.Bash.Allow) {
		return fmt.Sprintf("command not allowed by tools.bash.allow: %q", in.Command), true
	}
	cwd := r.cfg.Bash.Cwd
	if cwd == "" {
		if r.cfg.Files.Root != "" {
			cwd = r.cfg.Files.Root
		}
	}
	timeout := time.Duration(in.Timeout)
	if timeout <= 0 {
		timeout = time.Duration(r.cfg.Bash.TimeoutS)
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", in.Command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Sprintf("exit %d\n%s", ee.ExitCode(), text), true
		}
		return fmt.Sprintf("exec error: %v\n%s", err, text), true
	}
	return text, false
}

func bashAllowed(cmd string, allow []string) bool {
	for _, p := range allow {
		if p == "*" {
			return true
		}
		if matched, _ := filepath.Match(p, cmd); matched {
			return true
		}
		// prefix match so "git *" covers "git status -sb".
		if strings.HasSuffix(p, "*") && strings.HasPrefix(cmd, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return len(allow) == 0 // no allowlist = allow everything inside the root
}

// ---- files ---------------------------------------------------------------

func (r *Registry) resolve(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	root := r.cfg.Files.Root
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(abs, filepath.Clean(path))
	if !strings.HasPrefix(p, abs+string(filepath.Separator)) && p != abs {
		return "", fmt.Errorf("path escapes tools.files.root: %s", path)
	}
	return p, nil
}

func (r *Registry) execLS(input json.RawMessage) (string, bool) {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	p, err := r.resolve(in.Path)
	if err != nil {
		return err.Error(), true
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return fmt.Sprintf("ls %s: %v", in.Path, err), true
	}
	var b strings.Builder
	for _, e := range entries {
		marker := ""
		if e.IsDir() {
			marker = "/"
		}
		b.WriteString(e.Name() + marker + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), false
}

func (r *Registry) execGlob(input json.RawMessage) (string, bool) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fmt.Sprintf("bad Glob input: %v", err), true
	}
	base, err := r.resolve(in.Path)
	if err != nil {
		return err.Error(), true
	}
	var hits []string
	if strings.Contains(in.Pattern, "**") {
		err = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			if matched, _ := filepath.Match(in.Pattern, filepath.ToSlash(rel)); matched {
				hits = append(hits, rel)
			}
			return nil
		})
	} else {
		var m []string
		m, err = filepath.Glob(filepath.Join(base, in.Pattern))
		for _, x := range m {
			if rel, rerr := filepath.Rel(base, x); rerr == nil {
				hits = append(hits, rel)
			}
		}
	}
	if err != nil {
		return fmt.Sprintf("glob %s: %v", in.Pattern, err), true
	}
	if len(hits) == 0 {
		return "no matches", false
	}
	return strings.Join(hits, "\n"), false
}

var grepCompiled = atomic.Value{} // cache last-used regex

func (r *Registry) execGrep(input json.RawMessage) (string, bool) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fmt.Sprintf("bad Grep input: %v", err), true
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return fmt.Sprintf("bad regex: %v", err), true
	}
	base, err := r.resolve(in.Path)
	if err != nil {
		return err.Error(), true
	}
	var hits []string
	err = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if in.Glob != "" {
			if matched, _ := filepath.Match(in.Glob, filepath.Base(p)); !matched {
				return nil
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", p, i+1, line))
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Sprintf("grep: %v", err), true
	}
	if len(hits) == 0 {
		return "no matches", false
	}
	return strings.Join(hits, "\n"), false
}

func (r *Registry) execRead(input json.RawMessage) (string, bool) {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fmt.Sprintf("bad Read input: %v", err), true
	}
	path := in.FilePath
	if path == "" {
		path = in.Path
	}
	p, err := r.resolve(path)
	if err != nil {
		return err.Error(), true
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Sprintf("read %s: %v", path, err), true
	}
	lines := strings.Split(string(data), "\n")
	if in.Offset > 0 && in.Offset <= len(lines) {
		lines = lines[in.Offset-1:]
	}
	if in.Limit > 0 && in.Limit < len(lines) {
		lines = lines[:in.Limit]
	}
	return strings.Join(lines, "\n"), false
}

func (r *Registry) execWrite(input json.RawMessage) (string, bool) {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fmt.Sprintf("bad Write input: %v", err), true
	}
	path := in.FilePath
	if path == "" {
		path = in.Path
	}
	p, err := r.resolve(path)
	if err != nil {
		return err.Error(), true
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err.Error(), true
	}
	if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
		return fmt.Sprintf("write %s: %v", path, err), true
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), path), false
}

func (r *Registry) execEdit(input json.RawMessage) (string, bool) {
	var in struct {
		FilePath  string `json:"file_path"`
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fmt.Sprintf("bad Edit input: %v", err), true
	}
	path := in.FilePath
	if path == "" {
		path = in.Path
	}
	p, err := r.resolve(path)
	if err != nil {
		return err.Error(), true
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Sprintf("read %s: %v", path, err), true
	}
	text := string(data)
	if !strings.Contains(text, in.OldString) {
		return fmt.Sprintf("old_string not found in %s", path), true
	}
	if err := os.WriteFile(p, []byte(strings.Replace(text, in.OldString, in.NewString, 1)), 0o644); err != nil {
		return fmt.Sprintf("write %s: %v", path, err), true
	}
	return fmt.Sprintf("edited %s", path), false
}
