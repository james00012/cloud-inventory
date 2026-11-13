// Package install puts a provider on disk and refuses to run one it cannot
// verify. The checks run in order and stop at the first failure, as OpenTofu's
// provider installer does, and there is no path that warns and continues.
package install

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Check names the step that refused, so an error says which one failed rather
// than only that something did.
type Check string

const (
	Signature Check = "signature"
	Checksum  Check = "checksum"
	LockFile  Check = "lock file"
	Launch    Check = "launch check"
)

// Refused is what every failure is. A host cannot mistake it for a warning.
type Refused struct {
	Check  Check
	Detail string
}

func (r *Refused) Error() string { return fmt.Sprintf("refused at the %s: %s", r.Check, r.Detail) }

// Release is what CI publishes for one provider version: a zip per platform, a
// signed list of hashes, and the detached signature over that list.
type Release struct {
	Dir      string
	Provider string
	Version  string
}

func (r Release) zipName(goos, arch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", r.Provider, r.Version, goos, arch)
}

func (r Release) sumsPath() string { return filepath.Join(r.Dir, "SHA256SUMS") }
func (r Release) sigPath() string  { return filepath.Join(r.Dir, "SHA256SUMS.sig") }

// Install verifies a release and unpacks it into the shared cache, recording or
// matching its hashes in the lock file. It returns the binary's path.
func Install(release Release, goos, arch string, trusted []ed25519.PublicKey, cache string, lock *Lock) (string, error) {
	sums, err := os.ReadFile(release.sumsPath())
	if err != nil {
		return "", &Refused{Check: Signature, Detail: "no SHA256SUMS: " + err.Error()}
	}
	signature, err := os.ReadFile(release.sigPath())
	if err != nil {
		return "", &Refused{Check: Signature, Detail: "no SHA256SUMS.sig: " + err.Error()}
	}

	// 1. the signature over the list of hashes verifies with a trusted key
	if !anyKeyVerifies(trusted, sums, signature) {
		return "", &Refused{Check: Signature, Detail: "no trusted key signed SHA256SUMS"}
	}

	// 2. the zip's hash matches its signed line
	name := release.zipName(goos, arch)
	want, ok := signedHashes(sums)[name]
	if !ok {
		return "", &Refused{Check: Checksum, Detail: name + " has no signed line"}
	}
	zipPath := filepath.Join(release.Dir, name)
	got, err := fileHash(zipPath)
	if err != nil {
		return "", &Refused{Check: Checksum, Detail: err.Error()}
	}
	if got != want {
		return "", &Refused{Check: Checksum, Detail: fmt.Sprintf("%s hashes to %s, not the signed %s", name, short(got), short(want))}
	}

	into := filepath.Join(cache, release.Provider, release.Version, goos+"_"+arch)
	binary, err := unpack(zipPath, into, release.Provider)
	if err != nil {
		return "", err
	}

	// 3. the lock file: first use records the hashes, later uses must match
	contents, err := dirHash(into)
	if err != nil {
		return "", err
	}
	if err := lock.recordOrMatch(release.Provider, release.Version, "zh:"+got, "h1:"+contents); err != nil {
		return "", err
	}
	return binary, nil
}

// Verify is the check before every launch: the cached binary still hashes to
// what the lock file recorded.
func Verify(release Release, goos, arch, cache string, lock *Lock) (string, error) {
	into := filepath.Join(cache, release.Provider, release.Version, goos+"_"+arch)
	binary := filepath.Join(into, release.Provider)
	if _, err := os.Stat(binary); err != nil {
		return "", &Refused{Check: Launch, Detail: "not in the cache: " + err.Error()}
	}

	contents, err := dirHash(into)
	if err != nil {
		return "", &Refused{Check: Launch, Detail: err.Error()}
	}
	entry, ok := lock.Entry(release.Provider)
	if !ok {
		return "", &Refused{Check: Launch, Detail: "the lock file has no entry for " + release.Provider}
	}
	if !entry.has("h1:" + contents) {
		return "", &Refused{Check: Launch, Detail: "the cached build does not match the lock file"}
	}
	return binary, nil
}

// DevelopmentOverride points a provider at a local build. It skips every check,
// which is why it returns the warning a host must print on every run.
func DevelopmentOverride(provider, binary string) (string, string) {
	return binary, fmt.Sprintf("WARNING: %s is a development override, and no signature, checksum or lock check was run", provider)
}

func anyKeyVerifies(trusted []ed25519.PublicKey, message, signature []byte) bool {
	for _, key := range trusted {
		if ed25519.Verify(key, message, signature) {
			return true
		}
	}
	return false
}

// signedHashes reads the "<hash>  <name>" lines of a SHA256SUMS file.
func signedHashes(sums []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(sums), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 {
			out[parts[1]] = parts[0]
		}
	}
	return out
}

func fileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// dirHash is a hash over every unpacked file's name and contents, so changing
// any of them after the install is caught before the next launch.
func dirHash(dir string) (string, error) {
	var names []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		names = append(names, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)

	digest := sha256.New()
	for _, name := range names {
		rel, _ := filepath.Rel(dir, name)
		one, err := fileHash(name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(digest, "%s  %s\n", one, rel)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func unpack(zipPath, into, provider string) (string, error) {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", &Refused{Check: Checksum, Detail: "cannot read the zip: " + err.Error()}
	}
	defer archive.Close()

	if err := os.MkdirAll(into, 0o755); err != nil {
		return "", err
	}

	binary := ""
	for _, entry := range archive.File {
		name := filepath.Base(entry.Name)
		target := filepath.Join(into, name)

		source, err := entry.Open()
		if err != nil {
			return "", err
		}
		body, err := io.ReadAll(source)
		source.Close()
		if err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if name == provider {
			mode = 0o755
			binary = target
		}
		if err := os.WriteFile(target, body, mode); err != nil {
			return "", err
		}
	}
	if binary == "" {
		return "", &Refused{Check: Checksum, Detail: "the zip holds no " + provider + " binary"}
	}
	return binary, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
