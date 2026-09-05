package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// otherWritable is the group-write and other-write bits. Owner-write is fine —
// the point is that nobody ELSE can change what the engine executes.
const otherWritable os.FileMode = 0o022

// CheckPermissions enforces ADR-0003 §8: the engine refuses to run when the
// config file, or any configured plugin executable, is writable by other users.
//
// The reasoning the ADR gives is that the config is the trust boundary. Whoever
// can edit it can run arbitrary code as the engine's user, and whoever can
// overwrite a configured plugin binary gets exactly the same thing without ever
// touching the config. Both halves are cheap to check and are checked here.
//
// This is the cheap half of the security model and is not a sandbox. Two limits
// worth stating rather than implying away:
//
//   - It checks the files, not the directories holding them. A world-writable
//     directory still lets someone replace a 0644 file. ADR-0003 §8 scopes the
//     check to the config and the executables; directory hardening is
//     deployment advice, like the rest of the ADR's security stance.
//   - A command that cannot be found is not reported here. A missing plugin is
//     a run-time failure with a far clearer message than this check could give,
//     and refusing to start over it would turn a broken sink into a dead engine.
func (c *Config) CheckPermissions(configPath string) error {
	var problems []error

	if err := checkNotOtherWritable("config file", configPath); err != nil {
		problems = append(problems, err)
	}

	for _, id := range sortedKeys(c.Sources) {
		if cmd := c.Sources[id].Command; len(cmd) > 0 {
			what := fmt.Sprintf("plugin executable for source %q", id)
			if err := checkNotOtherWritable(what, resolve(cmd[0])); err != nil {
				problems = append(problems, err)
			}
		}
	}

	for _, id := range sortedKeys(c.Sinks) {
		if cmd := c.Sinks[id].Command; len(cmd) > 0 {
			what := fmt.Sprintf("plugin executable for sink %q", id)
			if err := checkNotOtherWritable(what, resolve(cmd[0])); err != nil {
				problems = append(problems, err)
			}
		}
	}

	return errors.Join(problems...)
}

// resolve turns a bare command name into the path that would actually run, so
// the check applies to the file the engine would execute rather than to a name.
// A name that resolves to nothing is returned unchanged and then fails the stat
// in checkNotOtherWritable, which treats it as not-a-finding.
func resolve(command string) string {
	if path, err := exec.LookPath(command); err == nil {
		return path
	}
	return command
}

// checkNotOtherWritable reports a file writable by group or other. The message
// carries the path, the mode and the fix, because the first person to read it
// will be reading a cron log and needs to act without going looking.
func checkNotOtherWritable(what, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		// Not a permission finding — see CheckPermissions' doc comment.
		return nil
	}

	mode := info.Mode().Perm()
	if mode&otherWritable == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s (%s) is writable by other users (mode %#o): whoever can write it can run arbitrary code as this user, so the engine refuses to start (ADR-0003 §8) — fix with: chmod go-w %s",
		what, path, mode, path)
}
