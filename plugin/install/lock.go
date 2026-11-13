package install

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Lock records the hashes a provider version was first installed with, so a
// later install of the same version on any machine must match. It follows
// OpenTofu's lock file in shape: a version and a set of hashes per provider.
type Lock struct {
	Path      string             `json:"-"`
	Providers map[string]*Record `json:"providers"`
}

// Record is one provider's locked version and hashes. A lock written on one
// platform can carry another's hashes, so a Mac lock verifies in Linux CI.
type Record struct {
	Version string   `json:"version"`
	Hashes  []string `json:"hashes"`
}

func (r *Record) has(hash string) bool {
	for _, known := range r.Hashes {
		if known == hash {
			return true
		}
	}
	return false
}

// Open reads a lock file, or starts an empty one where there is none.
func Open(path string) (*Lock, error) {
	lock := &Lock{Path: path, Providers: map[string]*Record{}}

	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return lock, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, lock); err != nil {
		return nil, fmt.Errorf("reading the lock file: %w", err)
	}
	if lock.Providers == nil {
		lock.Providers = map[string]*Record{}
	}
	lock.Path = path
	return lock, nil
}

func (l *Lock) Entry(provider string) (*Record, bool) {
	record, ok := l.Providers[provider]
	return record, ok
}

// recordOrMatch writes the hashes on first use and refuses a mismatch after.
func (l *Lock) recordOrMatch(provider, version string, hashes ...string) error {
	record, ok := l.Providers[provider]
	if !ok {
		sorted := append([]string(nil), hashes...)
		sort.Strings(sorted)
		l.Providers[provider] = &Record{Version: version, Hashes: sorted}
		return l.Save()
	}

	if record.Version != version {
		return &Refused{Check: LockFile,
			Detail: fmt.Sprintf("the lock file pins %s at %s, not %s", provider, record.Version, version)}
	}
	for _, hash := range hashes {
		if !record.has(hash) {
			return &Refused{Check: LockFile,
				Detail: fmt.Sprintf("%s does not match the locked hashes for %s %s", short(hash), provider, version)}
		}
	}
	return nil
}

// Save writes the lock file. A host commits it, as with OpenTofu's.
func (l *Lock) Save() error {
	if l.Path == "" {
		return nil
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.Path, append(body, '\n'), 0o644)
}
