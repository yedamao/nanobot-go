package sandbox

import (
	"path/filepath"
	"slices"
	"strings"
)

// System code and certificates command-line tools need, as in nanobot's
// _seatbelt. /var, /tmp, /Library and /etc are not granted wholesale: on
// macOS they hold host application data, not an isolated scratch
// filesystem as under bwrap. Seatbelt matches real (symlink-free) paths.
var (
	seatbeltReadSubpaths = []string{
		"/usr",
		"/bin",
		"/sbin",
		"/dev",
		"/System",
		"/Library/Developer/CommandLineTools",
		"/private/etc/ssl",
		"/private/var/db/dyld",
		"/private/var/db/timezone",
	}
	seatbeltReadLiterals = []string{
		"/private/etc/hosts",
		"/private/etc/services",
		"/private/etc/protocols",
		"/private/etc/gitconfig",
		"/etc/gitconfig",
		"/private/var/run/resolv.conf",
		"/private/var/select/sh",
		"/private/var/select/developer_dir",
		"/private/var/db/xcode_select_link",
		"/var/select/developer_dir",
		"/var/db/xcode_select_link",
	}
	// seatbeltTraversable are ancestors that must stay searchable but are
	// not covered by any allowed subpath: resolving /private/var/... stats
	// /private.
	seatbeltTraversable = []string{"/private"}
)

// seatbeltBackend runs commands under macOS sandbox-exec(1).
//
// Seatbelt has no mount namespace, so the workspace's parent is masked by a
// deny rule placed before the workspace allow: the last matching rule wins,
// which re-exposes the workspace while the rest of the parent stays denied.
// Unlike bwrap, the denied parent still appears in directory listings; only
// its contents are inaccessible. HOME and TMPDIR point at the workspace.
type seatbeltBackend struct{}

func (seatbeltBackend) Name() string    { return Seatbelt }
func (seatbeltBackend) GOOS() string    { return "darwin" }
func (seatbeltBackend) Program() string { return "/usr/bin/sandbox-exec" }

func (seatbeltBackend) Argv(program string, p Plan) ([]string, error) {
	// sandbox-exec has no --chdir. Exit before running any of the command
	// if cd fails; `cd ... && a; b` would still run b.
	script := "cd " + shellQuote(p.Cwd) + " || exit 1\n" + p.Command
	return []string{program, "-p", seatbeltProfile(p),
		"/usr/bin/env", "HOME=" + p.Workspace, "TMPDIR=" + p.Workspace,
		p.Shell, "-c", script}, nil
}

func (seatbeltBackend) ProbeArgv(program string) []string {
	return []string{program, "-p", "(version 1)\n(allow default)", "/usr/bin/true"}
}

func seatbeltProfile(p Plan) string {
	rules := []string{
		"(version 1)",
		"(deny default)",
		"(allow process*)",
		"(allow sysctl-read)",
		"(allow mach*)",
		"(allow ipc*)",
		"(allow network*)",
		// sh reads / while resolving paths; (subpath "/usr") does not cover it.
		`(allow file-read* (literal "/"))`,
		"(allow file-read* " + sbplList("subpath", seatbeltReadSubpaths) + ")",
		"(allow file-read* " + sbplList("literal", seatbeltReadLiterals) + ")",
		`(allow file-write* (literal "/dev/null") (literal "/dev/zero"))`,
	}

	// Mask the config directory; the workspace allow below re-exposes the
	// workspace subtree.
	if parent := filepath.Dir(p.Workspace); parent != "/" {
		rules = append(rules, "(deny file-read* file-write* (subpath "+sbplQuote(parent)+"))")
	}

	// Keep every ancestor of an allowed path searchable, including the
	// masked parent: metadata only, so it cannot be listed or read.
	traversable := ancestors(slices.Concat([]string{p.Workspace}, p.ROBinds, p.RWBinds,
		seatbeltReadSubpaths, seatbeltReadLiterals)...)
	var literals []string
	for _, d := range seatbeltTraversable {
		if !slices.Contains(traversable, d) {
			literals = append(literals, d)
		}
	}
	literals = append(literals, traversable...)
	rules = append(rules, "(allow file-read-metadata "+sbplList("literal", literals)+")")

	rules = append(rules, "(allow file-read* file-write* (subpath "+sbplQuote(p.Workspace)+"))")
	for _, d := range p.ROBinds {
		rules = append(rules,
			"(allow file-read* (subpath "+sbplQuote(d)+"))",
			// A read allow does not revoke a broader write grant.
			"(deny file-write* (subpath "+sbplQuote(d)+"))")
	}
	for _, d := range p.RWBinds {
		rules = append(rules, "(allow file-read* file-write* (subpath "+sbplQuote(d)+"))")
	}

	// Path-based read-only rules do not follow a renamed ancestor, so keep
	// those directory entries fixed. A RW bind covering a whole RO root
	// intentionally overrides its protection.
	var protected []string
	for _, ro := range p.ROBinds {
		if !slices.ContainsFunc(p.RWBinds, func(rw string) bool { return containsPath(rw, ro) }) {
			protected = append(protected, ro)
		}
	}
	if anc := ancestors(protected...); len(anc) > 0 {
		rules = append(rules, "(deny file-write-unlink "+sbplList("literal", anc)+")")
	}
	return strings.Join(rules, "\n")
}

func sbplList(kind string, paths []string) string {
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = "(" + kind + " " + sbplQuote(p) + ")"
	}
	return strings.Join(parts, " ")
}

// sbplQuote renders p as an SBPL string literal. SBPL uses C-style
// escapes, so an unescaped backslash or quote would end the literal early
// and change the meaning of every rule after it.
func sbplQuote(p string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(p, `\`, `\\`), `"`, `\"`) + `"`
}
