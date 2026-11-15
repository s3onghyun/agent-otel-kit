// Package backup records the state of an agent config file before setup
// changes it, so undo can put back the exact original bytes.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Record is what setup remembers about one config file.
type Record struct {
	Agent string `json:"agent"`
	// Path is the config file that setup wrote (symlinks resolved).
	Path string `json:"path"`
	// Existed is false when setup created the file.
	Existed bool `json:"existed"`
	// Mode is the original permission bits.
	Mode fs.FileMode `json:"mode"`
	// Original holds the original bytes (base64 in JSON).
	Original []byte `json:"original"`
	// CreatedDirs lists directories setup created, deepest first.
	CreatedDirs []string `json:"created_dirs,omitempty"`
	// WrittenSHA256 is the hash of the content setup last wrote.
	WrittenSHA256 string `json:"written_sha256"`
	SavedAt       string `json:"saved_at"`
}

// ErrNoRecord means setup has not changed this agent's config.
var ErrNoRecord = errors.New("no setup record")

// ErrChanged means the file was edited after setup wrote it.
var ErrChanged = errors.New("config file changed since setup")

// Store keeps records under a directory, one JSON file per agent.
type Store struct {
	Dir string
}

func (s Store) path(agent string) string {
	return filepath.Join(s.Dir, agent+".json")
}

// Load returns the record for agent.
func (s Store) Load(agent string) (*Record, error) {
	data, err := os.ReadFile(s.path(agent))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoRecord
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("read setup record %s: %w", s.path(agent), err)
	}
	return &r, nil
}

func (s Store) save(r *Record) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path(r.Agent), append(data, '\n'), 0o600)
}

func (s Store) remove(agent string) error {
	err := os.Remove(s.path(agent))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Hash returns the hex SHA-256 of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ResolvePath follows a symlinked config file to its target so the link
// itself survives the rewrite. A missing file resolves to itself.
func ResolvePath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil
	}
	return "", err
}

// ReadCurrent returns the file content, whether it exists, and its mode.
func ReadCurrent(path string) ([]byte, bool, fs.FileMode, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, 0, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, false, 0, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, 0, err
	}
	return data, true, fi.Mode().Perm(), nil
}

// Apply writes content to path. The first call for an agent records the
// original state; later calls keep that first record so undo always goes
// back to the state before any setup run.
func (s Store) Apply(agent, path string, content []byte) error {
	path, err := ResolvePath(path)
	if err != nil {
		return err
	}
	rec, err := s.Load(agent)
	switch {
	case errors.Is(err, ErrNoRecord):
		orig, existed, mode, err := ReadCurrent(path)
		if err != nil {
			return err
		}
		created, err := mkdirAllRecord(filepath.Dir(path))
		if err != nil {
			return err
		}
		rec = &Record{
			Agent:       agent,
			Path:        path,
			Existed:     existed,
			Mode:        mode,
			Original:    orig,
			CreatedDirs: created,
			SavedAt:     time.Now().UTC().Format(time.RFC3339),
		}
	case err != nil:
		return err
	default:
		if rec.Path != path {
			return fmt.Errorf("setup record for %s points at %s, not %s; run setup --undo %s first", agent, rec.Path, path, agent)
		}
	}
	mode := rec.Mode
	if !rec.Existed {
		mode = 0o600
	}
	if _, existed, curMode, err := ReadCurrent(path); err == nil && existed {
		mode = curMode
	}
	rec.WrittenSHA256 = Hash(content)
	// Save the record first: if the write below fails, undo still works.
	if err := s.save(rec); err != nil {
		return err
	}
	return writeAtomic(path, content, mode)
}

// Undo restores the original file recorded for agent. Unless force is set
// it refuses when the file no longer matches what setup wrote, so later
// hand edits are not silently thrown away.
func (s Store) Undo(agent string, force bool) (*Record, error) {
	rec, err := s.Load(agent)
	if err != nil {
		return nil, err
	}
	cur, existed, _, err := ReadCurrent(rec.Path)
	if err != nil {
		return nil, err
	}
	if !force && (!existed || Hash(cur) != rec.WrittenSHA256) {
		return rec, ErrChanged
	}
	if rec.Existed {
		if err := writeAtomic(rec.Path, rec.Original, rec.Mode); err != nil {
			return nil, err
		}
		// writeAtomic creates the file with the umask applied; set the
		// recorded bits exactly.
		if err := os.Chmod(rec.Path, rec.Mode); err != nil {
			return nil, err
		}
	} else {
		if err := os.Remove(rec.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, d := range rec.CreatedDirs {
			// Only removes empty directories; anything else stays.
			_ = os.Remove(d)
		}
	}
	return rec, s.remove(agent)
}

// mkdirAllRecord creates dir and its missing parents and returns the
// directories it created, deepest first.
func mkdirAllRecord(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return missing, nil
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".agent-otel-kit-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
