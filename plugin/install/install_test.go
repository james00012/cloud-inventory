package install

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publisher builds a release the way CI would: a zip, a list of hashes, and a
// detached signature over that list.
type publisher struct {
	t       *testing.T
	dir     string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newPublisher(t *testing.T) *publisher {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &publisher{t: t, dir: t.TempDir(), public: public, private: private}
}

func (p *publisher) release(provider, version, contents string) Release {
	p.t.Helper()

	name := provider + "_" + version + "_darwin_arm64.zip"
	path := filepath.Join(p.dir, name)

	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create(provider)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := file.Write([]byte(contents)); err != nil {
		p.t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		p.t.Fatal(err)
	}

	p.sign(name, buffer.Bytes())
	return Release{Dir: p.dir, Provider: provider, Version: version}
}

func (p *publisher) sign(name string, body []byte) {
	p.t.Helper()
	digest := sha256.Sum256(body)
	sums := []byte(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	if err := os.WriteFile(filepath.Join(p.dir, "SHA256SUMS"), sums, 0o644); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "SHA256SUMS.sig"), ed25519.Sign(p.private, sums), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func refusedAt(t *testing.T, err error, want Check) {
	t.Helper()
	var refused *Refused
	if !errors.As(err, &refused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if refused.Check != want {
		t.Fatalf("refused at the %s, want the %s: %v", refused.Check, want, err)
	}
	if !strings.Contains(err.Error(), string(want)) {
		t.Fatalf("the error does not name the check: %v", err)
	}
}

func TestAVerifiedReleaseInstallsAndLaunches(t *testing.T) {
	p := newPublisher(t)
	release := p.release("provider-gcp", "1.0.0", "the real binary")
	cache := t.TempDir()
	lock, _ := Open(filepath.Join(t.TempDir(), "providers.lock.json"))

	binary, err := Install(release, "darwin", "arm64", []ed25519.PublicKey{p.public}, cache, lock)
	if err != nil {
		t.Fatalf("installing: %v", err)
	}
	if body, _ := os.ReadFile(binary); string(body) != "the real binary" {
		t.Fatalf("unpacked %q", body)
	}

	// First use records the hashes, and the launch check reads them back.
	entry, ok := lock.Entry("provider-gcp")
	if !ok || len(entry.Hashes) != 2 {
		t.Fatalf("the lock file recorded %+v", entry)
	}
	if _, err := Verify(release, "darwin", "arm64", cache, lock); err != nil {
		t.Fatalf("the launch check refused a build it had just installed: %v", err)
	}
}

func TestAReleaseSignedByAnUntrustedKeyIsRefused(t *testing.T) {
	p := newPublisher(t)
	release := p.release("provider-gcp", "1.0.0", "the real binary")
	other, _, _ := ed25519.GenerateKey(nil)
	lock, _ := Open(filepath.Join(t.TempDir(), "lock.json"))

	_, err := Install(release, "darwin", "arm64", []ed25519.PublicKey{other}, t.TempDir(), lock)
	refusedAt(t, err, Signature)
}

func TestAMissingSignatureIsRefusedRatherThanSkipped(t *testing.T) {
	p := newPublisher(t)
	release := p.release("provider-gcp", "1.0.0", "the real binary")
	if err := os.Remove(filepath.Join(p.dir, "SHA256SUMS.sig")); err != nil {
		t.Fatal(err)
	}
	lock, _ := Open(filepath.Join(t.TempDir(), "lock.json"))

	// This is the case where Terragrunt's engine loader runs the engine
	// anyway. Here it stops.
	_, err := Install(release, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), lock)
	refusedAt(t, err, Signature)
}

func TestATamperedZipIsRefusedAtTheChecksum(t *testing.T) {
	p := newPublisher(t)
	release := p.release("provider-gcp", "1.0.0", "the real binary")

	// The zip is swapped after it was signed, for another valid zip, so the
	// signature still verifies and nothing but the checksum catches it.
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create("provider-gcp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("a binary nobody signed")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir, "provider-gcp_1.0.0_darwin_arm64.zip")
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, _ := Open(filepath.Join(t.TempDir(), "lock.json"))

	_, err = Install(release, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), lock)
	refusedAt(t, err, Checksum)
}

// TestADifferentVersionThanTheLockFilePinsIsRefused covers the other half of
// the lock: not only the hashes, but which version they belong to.
func TestADifferentVersionThanTheLockFilePinsIsRefused(t *testing.T) {
	p := newPublisher(t)
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	first := p.release("provider-gcp", "1.0.0", "the real binary")
	lock, _ := Open(lockPath)
	if _, err := Install(first, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), lock); err != nil {
		t.Fatalf("installing: %v", err)
	}

	second := p.release("provider-gcp", "2.0.0", "the next binary")
	reopened, _ := Open(lockPath)
	_, err := Install(second, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), reopened)
	refusedAt(t, err, LockFile)
	if !strings.Contains(err.Error(), "pins provider-gcp at 1.0.0") {
		t.Fatalf("the error does not name the pinned version: %v", err)
	}
}

func TestARebuiltReleaseAtTheSameVersionIsRefusedByTheLockFile(t *testing.T) {
	p := newPublisher(t)
	cache := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	first := p.release("provider-gcp", "1.0.0", "the real binary")
	lock, _ := Open(lockPath)
	if _, err := Install(first, "darwin", "arm64", []ed25519.PublicKey{p.public}, cache, lock); err != nil {
		t.Fatalf("installing: %v", err)
	}

	// The same version, published again with different contents and a valid
	// signature. The lock file is the only thing that catches this.
	second := p.release("provider-gcp", "1.0.0", "a different binary")
	reopened, _ := Open(lockPath)
	_, err := Install(second, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), reopened)
	refusedAt(t, err, LockFile)
}

func TestACachedBuildChangedAfterInstallIsRefusedAtLaunch(t *testing.T) {
	p := newPublisher(t)
	release := p.release("provider-gcp", "1.0.0", "the real binary")
	cache := t.TempDir()
	lock, _ := Open(filepath.Join(t.TempDir(), "lock.json"))

	binary, err := Install(release, "darwin", "arm64", []ed25519.PublicKey{p.public}, cache, lock)
	if err != nil {
		t.Fatalf("installing: %v", err)
	}

	// Something edits the cache between the install and the launch.
	if err := os.WriteFile(binary, []byte("swapped in the cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = Verify(release, "darwin", "arm64", cache, lock)
	refusedAt(t, err, Launch)
}

func TestADevelopmentOverrideSkipsEveryCheckAndSaysSo(t *testing.T) {
	binary, warning := DevelopmentOverride("provider-gcp", "/tmp/my-build")

	if binary != "/tmp/my-build" {
		t.Fatalf("pointed at %q", binary)
	}
	for _, named := range []string{"signature", "checksum", "lock"} {
		if !strings.Contains(warning, named) {
			t.Fatalf("the warning does not say the %s check was skipped: %q", named, warning)
		}
	}
}

// TestALockFileFromOneMachineVerifiesOnAnother is why the lock file carries
// hashes rather than a path: CI installs into its own cache and still matches.
func TestALockFileFromOneMachineVerifiesOnAnother(t *testing.T) {
	p := newPublisher(t)
	release := p.release("provider-gcp", "1.0.0", "the real binary")
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	onALaptop, _ := Open(lockPath)
	if _, err := Install(release, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), onALaptop); err != nil {
		t.Fatalf("installing: %v", err)
	}

	inCI, err := Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(release, "darwin", "arm64", []ed25519.PublicKey{p.public}, t.TempDir(), inCI); err != nil {
		t.Fatalf("the committed lock file refused the same release: %v", err)
	}
}
