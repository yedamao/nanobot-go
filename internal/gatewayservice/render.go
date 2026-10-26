package gatewayservice

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
)

// plist is a LaunchAgent's settings.
type plist struct {
	Label            string
	ProgramArguments []string
	WorkingDirectory string
	RunAtLoad        bool
	// Path is the service's PATH (MCP servers and tools run from it);
	// empty leaves launchd's default.
	Path           string
	Stdout, Stderr string
}

// renderPlist renders p as a launchd property list. KeepAlive
// {SuccessfulExit: false} restarts the gateway when it exits with an
// error, not after a clean shutdown (SIGTERM from `gateway stop`).
func renderPlist(p plist) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { b.WriteString("\t<key>" + k + "</key>\n") }
	str := func(indent, v string) { b.WriteString(indent + "<string>" + xmlEscape(v) + "</string>\n") }
	key("Label")
	str("\t", p.Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range p.ProgramArguments {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	key("WorkingDirectory")
	str("\t", p.WorkingDirectory)
	key("RunAtLoad")
	if p.RunAtLoad {
		b.WriteString("\t<true/>\n")
	} else {
		b.WriteString("\t<false/>\n")
	}
	key("KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	if p.Path != "" {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n\t\t<key>PATH</key>\n")
		str("\t\t", p.Path)
		b.WriteString("\t</dict>\n")
	}
	key("StandardOutPath")
	str("\t", p.Stdout)
	key("StandardErrorPath")
	str("\t", p.Stderr)
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// renderUnit renders a systemd user unit running argv, restarted 10
// seconds after it exits for any reason.
func renderUnit(description string, argv []string, workDir, path string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = systemdQuote(a, true)
	}
	lines := []string{
		"[Unit]",
		"Description=" + description,
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		// WorkingDirectory takes the rest of the line unquoted.
		"WorkingDirectory=" + strings.ReplaceAll(workDir, "%", "%%"),
		"ExecStart=" + strings.Join(quoted, " "),
		"Restart=always",
		"RestartSec=10",
	}
	if path != "" {
		lines = append(lines, "Environment="+systemdQuote("PATH="+path, false))
	}
	lines = append(lines,
		"NoNewPrivileges=yes",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	)
	return strings.Join(lines, "\n")
}

var needsQuote = regexp.MustCompile(`\s|['"\\%$]`)

// systemdQuote quotes v as one word of an ExecStart= or Environment=
// line when it holds whitespace, quotes, backslashes or the specifier
// character %; dollar also escapes $, which ExecStart= expands.
func systemdQuote(v string, dollar bool) string {
	if v != "" && !needsQuote.MatchString(v) {
		return v
	}
	pairs := []string{`\`, `\\`, `"`, `\"`, "%", "%%"}
	if dollar {
		pairs = append(pairs, "$", "$$")
	}
	return `"` + strings.NewReplacer(pairs...).Replace(v) + `"`
}

// plistArguments returns the ProgramArguments strings of a plist.
func plistArguments(data []byte) ([]string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	var (
		depth   int
		lastKey string
		inArgs  bool
		argv    []string
	)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return argv, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			// plist > dict > key|array: the top-level dict is depth 2.
			switch {
			case depth == 3 && t.Name.Local == "key":
				var k string
				if err := d.DecodeElement(&k, &t); err != nil {
					return nil, err
				}
				depth--
				lastKey = k
			case depth == 3 && t.Name.Local == "array" && lastKey == "ProgramArguments":
				inArgs = true
			case depth == 4 && inArgs && t.Name.Local == "string":
				var s string
				if err := d.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				depth--
				argv = append(argv, s)
			}
		case xml.EndElement:
			if depth == 3 && inArgs {
				return argv, nil
			}
			depth--
		}
	}
}

// unitExecStart returns the words of a unit's ExecStart= line, undoing
// systemdQuote.
func unitExecStart(unit string) ([]string, error) {
	for _, line := range strings.Split(unit, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		var (
			argv           []string
			cur            strings.Builder
			inWord, quoted bool
		)
		r := []rune(v)
		for i := 0; i < len(r); i++ {
			c := r[i]
			switch {
			case quoted && c == '\\' && i+1 < len(r):
				i++
				cur.WriteRune(r[i])
			case quoted && c == '"':
				quoted = false
			case (c == '%' || c == '$') && i+1 < len(r) && r[i+1] == c:
				i++
				cur.WriteRune(c)
				inWord = true
			case !quoted && c == '"':
				quoted, inWord = true, true
			case !quoted && (c == ' ' || c == '\t'):
				if inWord {
					argv = append(argv, cur.String())
					cur.Reset()
					inWord = false
				}
			default:
				cur.WriteRune(c)
				inWord = true
			}
		}
		if quoted {
			return nil, errors.New("unterminated quote in ExecStart")
		}
		if inWord {
			argv = append(argv, cur.String())
		}
		return argv, nil
	}
	return nil, errors.New("no ExecStart line")
}
