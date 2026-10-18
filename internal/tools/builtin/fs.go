package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

const (
	defaultReadLimit = 2000 // lines returned by read_file by default
	maxLineChars     = 2000 // longer lines are cut
	maxDirEntries    = 1000
	binarySniffBytes = 8000
)

// ReadFile implements read_file.
type ReadFile struct{ ws *security.Workspace }

// NewReadFile returns the read_file tool.
func NewReadFile(ws *security.Workspace) *ReadFile { return &ReadFile{ws: ws} }

func (*ReadFile) Name() string { return "read_file" }
func (*ReadFile) Description() string {
	return "Read a text file. Output is line-numbered (\"N| line\"); use offset/limit to page through large files."
}
func (*ReadFile) ReadOnly() bool { return true }
func (*ReadFile) Parameters() map[string]any {
	return tools.Object(map[string]any{
		"path":   tools.String("File path, relative to the workspace or absolute."),
		"offset": tools.Integer("1-based line number to start from (default 1)."),
		"limit":  tools.Integer(fmt.Sprintf("Maximum number of lines to return (default %d).", defaultReadLimit)),
	}, "path")
}

func (t *ReadFile) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if args.Offset < 1 {
		args.Offset = 1
	}
	if args.Limit <= 0 {
		args.Limit = defaultReadLimit
	}
	p, err := t.ws.ResolveRead(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return tools.Errorf("read %s: %v", args.Path, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return tools.Errorf("%s is a directory; use list_dir", args.Path)
	}

	br := bufio.NewReader(f)
	if head, _ := br.Peek(binarySniffBytes); bytes.IndexByte(head, 0) >= 0 {
		return tools.Errorf("%s appears to be a binary file", args.Path)
	}

	var out strings.Builder
	total, shown := 0, 0
	for {
		if total%1024 == 0 && ctx.Err() != nil {
			return tools.Errorf("read %s: %v", args.Path, ctx.Err())
		}
		line, err := br.ReadString('\n')
		if line != "" {
			total++
			if total >= args.Offset && shown < args.Limit {
				line = strings.TrimRight(line, "\r\n")
				if r := []rune(line); len(r) > maxLineChars {
					line = string(r[:maxLineChars]) + "... (line truncated)"
				}
				fmt.Fprintf(&out, "%d| %s\n", total, line)
				shown++
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return tools.Errorf("read %s: %v", args.Path, err)
		}
	}

	switch {
	case total == 0:
		return tools.Text("(empty file)")
	case shown == 0:
		return tools.Errorf("offset %d is past the end of %s (%d lines)", args.Offset, args.Path, total)
	}
	if last := args.Offset + shown - 1; args.Offset > 1 || last < total {
		fmt.Fprintf(&out, "\n(showing lines %d-%d of %d", args.Offset, last, total)
		if last < total {
			fmt.Fprintf(&out, "; use offset=%d to continue", last+1)
		}
		out.WriteString(")")
	}
	return tools.Text(strings.TrimRight(out.String(), "\n"))
}

// WriteFile implements write_file.
type WriteFile struct{ ws *security.Workspace }

// NewWriteFile returns the write_file tool.
func NewWriteFile(ws *security.Workspace) *WriteFile { return &WriteFile{ws: ws} }

func (*WriteFile) Name() string { return "write_file" }
func (*WriteFile) Description() string {
	return "Create or overwrite a file with the given content. Parent directories are created as needed."
}
func (*WriteFile) ReadOnly() bool { return false }
func (*WriteFile) Parameters() map[string]any {
	return tools.Object(map[string]any{
		"path":    tools.String("File path, relative to the workspace or absolute."),
		"content": tools.String("Full file content."),
	}, "path", "content")
}

func (t *WriteFile) Execute(_ context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	p, err := t.ws.ResolveWrite(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return tools.Errorf("%s is a directory", args.Path)
	}
	if err := atomicWrite(p, []byte(args.Content)); err != nil {
		return tools.Errorf("write %s: %v", args.Path, err)
	}
	return tools.Text(fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), t.ws.Rel(p)))
}

// atomicWrite writes data to a temp file in the target directory and
// renames it into place, preserving the mode of an existing file.
func atomicWrite(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// EditFile implements edit_file.
type EditFile struct{ ws *security.Workspace }

// NewEditFile returns the edit_file tool.
func NewEditFile(ws *security.Workspace) *EditFile { return &EditFile{ws: ws} }

func (*EditFile) Name() string { return "edit_file" }
func (*EditFile) Description() string {
	return "Replace old_string with new_string in a file. old_string must match exactly one location " +
		"unless replace_all is true; include surrounding lines to make it unique."
}
func (*EditFile) ReadOnly() bool { return false }
func (*EditFile) Parameters() map[string]any {
	return tools.Object(map[string]any{
		"path":        tools.String("File path, relative to the workspace or absolute."),
		"old_string":  tools.String("Exact text to replace."),
		"new_string":  tools.String("Replacement text."),
		"replace_all": tools.Boolean("Replace every occurrence (default false)."),
	}, "path", "old_string", "new_string")
}

func (t *EditFile) Execute(_ context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if args.OldString == "" {
		return tools.Errorf("old_string must not be empty; use write_file to create a file")
	}
	if args.OldString == args.NewString {
		return tools.Errorf("old_string and new_string are identical")
	}
	p, err := t.ws.ResolveWrite(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return tools.Errorf("read %s: %v", args.Path, err)
	}
	content := string(data)

	matches, fuzzy := findMatches(content, args.OldString)
	switch {
	case len(matches) == 0:
		return tools.Errorf("old_string not found in %s. Read the file and copy the exact text, including whitespace.", args.Path)
	case len(matches) > 1 && !args.ReplaceAll:
		lines := make([]string, len(matches))
		for i, m := range matches {
			lines[i] = fmt.Sprint(m.line)
		}
		return tools.Errorf("old_string matches %d locations in %s (lines %s). Add surrounding context to make it unique, or set replace_all=true.",
			len(matches), args.Path, strings.Join(lines, ", "))
	}

	replacement := args.NewString
	if fuzzy && strings.Contains(content, "\r\n") && !strings.Contains(replacement, "\r\n") {
		replacement = strings.ReplaceAll(replacement, "\n", "\r\n")
	}
	var b strings.Builder
	prev := 0
	for _, m := range matches {
		b.WriteString(content[prev:m.start])
		b.WriteString(replacement)
		prev = m.end
	}
	b.WriteString(content[prev:])

	if err := atomicWrite(p, []byte(b.String())); err != nil {
		return tools.Errorf("write %s: %v", args.Path, err)
	}
	msg := fmt.Sprintf("Edited %s: replaced %d occurrence(s)", t.ws.Rel(p), len(matches))
	if fuzzy {
		msg += " (matched ignoring leading/trailing whitespace)"
	}
	return tools.Text(msg)
}

type matchSpan struct {
	start, end int // byte offsets in content
	line       int // 1-based line of start
}

// findMatches locates old in content exactly, falling back to a line-wise
// comparison that ignores leading/trailing whitespace on each line (like
// nanobot's _find_trim_matches). fuzzy reports whether the fallback was used.
// Returned spans never overlap.
func findMatches(content, old string) (spans []matchSpan, fuzzy bool) {
	for start := 0; ; {
		i := strings.Index(content[start:], old)
		if i < 0 {
			break
		}
		i += start
		spans = append(spans, matchSpan{i, i + len(old), strings.Count(content[:i], "\n") + 1})
		start = i + len(old)
	}
	if len(spans) > 0 {
		return spans, false
	}
	return findTrimMatches(content, old), true
}

func findTrimMatches(content, old string) []matchSpan {
	oldLines := splitLines(old)
	if len(oldLines) == 0 {
		return nil
	}
	want := make([]string, len(oldLines))
	for i, l := range oldLines {
		want[i] = strings.TrimSpace(l)
	}
	if strings.Join(want, "") == "" {
		return nil // whitespace-only needle would match everywhere
	}

	// offsets[i] is the byte offset of line i; lines keep their terminators.
	var lines []string
	var offsets []int
	for pos := 0; pos < len(content); {
		end := strings.IndexByte(content[pos:], '\n')
		if end < 0 {
			end = len(content)
		} else {
			end += pos + 1
		}
		lines = append(lines, content[pos:end])
		offsets = append(offsets, pos)
		pos = end
	}
	offsets = append(offsets, len(content))

	var spans []matchSpan
	for i := 0; i+len(want) <= len(lines); i++ {
		ok := true
		for j, w := range want {
			if strings.TrimSpace(lines[i+j]) != w {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		start, end := offsets[i], offsets[i+len(want)]
		// Include the final line terminator only if old_string had one.
		if !strings.HasSuffix(old, "\n") {
			last := lines[i+len(want)-1]
			end -= len(last) - len(strings.TrimRight(last, "\r\n"))
		}
		spans = append(spans, matchSpan{start, end, i + 1})
		i += len(want) - 1 // no overlapping matches
	}
	return spans
}

// splitLines splits on "\n" like Python's str.splitlines, without a
// trailing empty element.
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ListDir implements list_dir.
type ListDir struct{ ws *security.Workspace }

// NewListDir returns the list_dir tool.
func NewListDir(ws *security.Workspace) *ListDir { return &ListDir{ws: ws} }

func (*ListDir) Name() string { return "list_dir" }
func (*ListDir) Description() string {
	return "List the entries of a directory. Directories end with \"/\"."
}
func (*ListDir) ReadOnly() bool { return true }
func (*ListDir) Parameters() map[string]any {
	return tools.Object(map[string]any{
		"path": tools.String("Directory path, relative to the workspace or absolute (default: workspace root)."),
	})
}

func (t *ListDir) Execute(_ context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Path string `json:"path"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if args.Path == "" {
		args.Path = "."
	}
	p, err := t.ws.ResolveRead(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return tools.Errorf("list %s: %v", args.Path, err)
	}
	if len(entries) == 0 {
		return tools.Text("(empty directory)")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		} else if e.Type()&fs.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(p, name)); err == nil && info.IsDir() {
				name += "/"
			}
		}
		names = append(names, name)
	}
	slices.Sort(names)
	extra := ""
	if len(names) > maxDirEntries {
		extra = fmt.Sprintf("\n... (%d more entries)", len(names)-maxDirEntries)
		names = names[:maxDirEntries]
	}
	return tools.Text(strings.Join(names, "\n") + extra)
}
