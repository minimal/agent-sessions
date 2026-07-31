package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

type trasher struct {
	lookPath    func(string) (string, error)
	run         func(string, ...string) error
	fallbackDir func() (string, error)
}

func systemTrasher() trasher {
	return trasher{
		lookPath: exec.LookPath,
		run: func(path string, args ...string) error {
			return exec.Command(path, args...).Run()
		},
		fallbackDir: trashFallbackDir,
	}
}

// Put moves paths to the first supported system Trash command. When none is
// installed, it moves them into an app-owned data directory instead.
func (t trasher) Put(paths ...string) error {
	existing, err := existingTrashPaths(paths)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return fmt.Errorf("no session files found")
	}

	commands := []struct {
		name   string
		prefix []string
	}{
		{name: "trash"},
		{name: "trash-put"},
		{name: "gio", prefix: []string{"trash"}},
	}
	for _, command := range commands {
		path, err := t.lookPath(command.name)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				continue
			}
			return fmt.Errorf("find %s: %w", command.name, err)
		}
		args := append(append([]string{}, command.prefix...), existing...)
		if err := t.run(path, args...); err != nil {
			return fmt.Errorf("%s: %w", command.name, err)
		}
		return nil
	}

	dir, err := t.fallbackDir()
	if err != nil {
		return fmt.Errorf("fallback trash: %w", err)
	}
	return moveToFallbackTrash(dir, existing)
}

func existingTrashPaths(paths []string) ([]string, error) {
	existing := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			return nil, fmt.Errorf("empty session path")
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", path, err)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Lstat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		existing = append(existing, path)
	}
	return existing, nil
}

func trashFallbackDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "agent-sessions", "trash"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "agent-sessions", "trash"), nil
}

func moveToFallbackTrash(root string, paths []string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}

	destinations := make(map[string]string, len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		if previous, exists := destinations[name]; exists {
			return fmt.Errorf("cannot trash both %s and %s: destination name %q is shared", previous, path, name)
		}
		destinations[name] = path
	}

	batch, err := os.MkdirTemp(root, "session-")
	if err != nil {
		return fmt.Errorf("create trash entry: %w", err)
	}
	type move struct {
		from string
		to   string
	}
	var moved []move
	for _, from := range paths {
		to := filepath.Join(batch, filepath.Base(from))
		if err := os.Rename(from, to); err != nil {
			var rollback []error
			for i := len(moved) - 1; i >= 0; i-- {
				if rollbackErr := os.Rename(moved[i].to, moved[i].from); rollbackErr != nil {
					rollback = append(rollback, rollbackErr)
				}
			}
			if len(rollback) == 0 {
				_ = os.Remove(batch)
				return fmt.Errorf("move %s to fallback trash: %w", from, err)
			}
			return fmt.Errorf("move %s to fallback trash: %w", from, errors.Join(append([]error{err}, rollback...)...))
		}
		moved = append(moved, move{from: from, to: to})
	}
	return nil
}
