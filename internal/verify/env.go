package verify

import (
	"slices"
	"strings"
)

// baseEnvKeys are the variables a build or test command needs to find its tools and
// write temporary files. Everything else (API keys, tokens, cloud credentials) stays out:
// the command runs code an agent just wrote.
var baseEnvKeys = []string{
	// Unix
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "TZ",
	// Windows (case-insensitive there)
	"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "WINDIR", "TEMP", "TMP",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES",
	"PROGRAMFILES(X86)", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
}

// MinimalEnv filters from (typically os.Environ()) down to the base variables, the
// locale variables (LC_*), and the extra keys a project allows. Later entries win, as
// with exec.Cmd.Env. Keys match case-insensitively so one list works on every OS.
func MinimalEnv(from []string, extra ...string) []string {
	allowed := make([]string, 0, len(baseEnvKeys)+len(extra))
	for _, k := range append(slices.Clone(baseEnvKeys), extra...) {
		allowed = append(allowed, strings.ToUpper(k))
	}
	var out []string
	for _, kv := range from {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		up := strings.ToUpper(k)
		if slices.Contains(allowed, up) || strings.HasPrefix(up, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}
