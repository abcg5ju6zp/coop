package cli

// Self-update replaces the running coop binary with a newer GitHub release. It
// downloads the versioned GoReleaser archive and checksum as data, verifies them
// locally, and never executes a downloaded installer.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// These are vars (not consts) so tests can point them at an httptest server.
var (
	githubLatestURL = "https://api.github.com/repos/AndrewDryga/coop/releases/latest"

	releaseFileURLFor = func(tag, name string) string {
		return "https://github.com/AndrewDryga/coop/releases/download/" + tag + "/" + name
	}

	// executablePath resolves the running binary; a var so tests can stub it.
	executablePath = os.Executable

	updateHTTPClient = &http.Client{Timeout: 30 * time.Second}

	// Release assets are megabytes over whatever link the host has, so they get a whole-transfer
	// bound of minutes rather than the API client's 30s — which timed out real updates on slow
	// connections — plus the explicit size caps below.
	releaseHTTPClient = &http.Client{Timeout: 10 * time.Minute}
)

// Release files are fully verified before use; the caps only keep a wrong or hostile response from
// exhausting memory before the checksum ever runs. A stripped coop binary is a few MB.
const (
	maxReleaseChecksumsBytes = 1 << 20
	maxReleaseArchiveBytes   = 256 << 20
	maxReleaseBinaryBytes    = 256 << 20
)

// checkError marks a soft failure: coop couldn't determine the latest release
// (offline, GitHub rate limit). In a combined `coop update` this is a warning, not
// a hard failure — the box rebuild still runs.
type checkError struct{ err error }

func (e checkError) Error() string { return e.err.Error() }
func (e checkError) Unwrap() error { return e.err }

// normalizeVersion drops a leading "v" and surrounding space so the ldflags
// version ("2.7.2") and a GitHub tag ("v2.7.2") compare equal.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// latestReleaseTag returns the tag_name of the newest GitHub release (e.g. "v2.7.3").
func latestReleaseTag() (string, error) {
	req, err := http.NewRequest(http.MethodGet, githubLatestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", errors.New("GitHub did not return a valid release version")
	}
	if rel.TagName == "" {
		return "", errors.New("GitHub did not return a valid release version")
	}
	return rel.TagName, nil
}

// selfUpdateOutcome is what the binary half of an update DID — which is what its caller has to
// report, and the one thing it must not guess at.
type selfUpdateOutcome int

const (
	selfUpdateDev       selfUpdateOutcome = iota // a dev/source build; self-update does not replace it
	selfUpdateCurrent                            // already the latest release
	selfUpdateAhead                              // newer than the latest release; kept
	selfUpdateInstalled                          // replaced with a newer release
	selfUpdateBehind                             // a newer release exists; the caller stages it
)

// selfUpdateResult carries the versions involved so the caller can name both without asking
// GitHub a second time.
type selfUpdateResult struct {
	Outcome   selfUpdateOutcome
	Current   string // this binary's version, normalized (no leading v)
	Latest    string // the latest release, normalized; "" when it was never resolved
	LatestTag string // the latest release tag verbatim, e.g. "v2.7.3"; "" when never resolved
}

// installFailure is a hard binary-install failure, carrying the sentence a person reads and
// whether the binary on disk is provably untouched. Unchanged is only true for a failure that
// happens BEFORE the atomic rename — nothing after it can undo an installation that happened.
type installFailure struct {
	reason    string
	unchanged bool
	err       error
}

func (e *installFailure) Error() string { return e.reason }
func (e *installFailure) Unwrap() error { return e.err }

// resolveSelfUpdate decides what the binary half of an update should do WITHOUT changing the
// installed executable, so both the combined update and --self-only act on one decision: dev /
// current / ahead are no-ops, behind names the executable the caller stages the candidate over,
// a check failure is a checkError (soft), and an executable-location failure is an installFailure
// (hard). Nothing here downloads an artifact.
func resolveSelfUpdate() (selfUpdateResult, string, error) {
	cur := resolveVersion()
	result := selfUpdateResult{Current: normalizeVersion(cur)}
	if !releaseVersion(cur) {
		result.Outcome = selfUpdateDev
		return result, "", nil
	}
	exe, err := resolveUpdateExe()
	if err != nil {
		return result, "", err
	}
	latest, err := latestReleaseTag()
	if err != nil {
		return result, "", checkError{err}
	}
	result.Latest = normalizeVersion(latest)
	result.LatestTag = latest
	switch compareReleaseVersions(cur, latest) {
	case releaseInvalid:
		return result, "", checkError{errors.New("GitHub did not return a valid release version")}
	case releaseEqual:
		result.Outcome = selfUpdateCurrent
	case releaseAhead:
		result.Outcome = selfUpdateAhead
	case releaseBehind:
		result.Outcome = selfUpdateBehind
	}
	return result, exe, nil
}

// dirWritable reports whether dir accepts new files (so the atomic install can stage a
// temp there), by creating and removing a probe file.
func dirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".coop-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// fetchVerifiedReleaseBinary downloads the checksum file and the GoReleaser archive for tag,
// verifies the archive against the published checksum, and extracts the Coop binary from it.
// Nothing here touches the installed executable: every failure is an installFailure that left
// it unchanged. The bytes are what a generation update stages as its candidate — verification
// happens here, before the candidate can be executed or activated.
func fetchVerifiedReleaseBinary(tag string) ([]byte, error) {
	version := normalizeVersion(tag)
	asset := fmt.Sprintf("coop_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	fail := func(reason string, err error) error {
		return &installFailure{reason: reason, unchanged: true, err: err}
	}
	checksums, err := fetchReleaseFile(tag, "checksums.txt", maxReleaseChecksumsBytes)
	if err != nil {
		return nil, fail("Could not download the release checksums: "+err.Error()+".", err)
	}
	archive, err := fetchReleaseFile(tag, asset, maxReleaseArchiveBytes)
	if err != nil {
		return nil, fail("Could not download "+asset+": "+err.Error()+".", err)
	}
	if err := verifyReleaseChecksum(asset, archive, checksums); err != nil {
		return nil, fail(sentence(err.Error()), err)
	}
	binary, err := releaseBinary(archive)
	if err != nil {
		return nil, fail(sentence(err.Error()), err)
	}
	return binary, nil
}

// stageReleaseBinary downloads and verifies tag and writes its binary to stagedPath (0755,
// fsynced) WITHOUT touching the installed executable. It returns the hex sha256 of the staged
// bytes, so an interrupted update can later prove the staged candidate is still this release
// instead of trusting or re-downloading it. Every failure leaves both the stage and the installed
// binary untouched.
func stageReleaseBinary(stagedPath, tag string) (string, error) {
	binary, err := fetchVerifiedReleaseBinary(tag)
	if err != nil {
		return "", err
	}
	if err := writeStagedBinary(stagedPath, binary); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(binary)), nil
}

// commitStagedBinary activates a staged candidate by atomically renaming it over exe — the single
// point at which the active binary changes. The rename is the commit: its failure leaves the
// installed binary provably unchanged, and nothing after a successful rename can undo it.
func commitStagedBinary(stagedPath, exe string) error {
	if err := os.Rename(stagedPath, exe); err != nil {
		return &installFailure{reason: "Could not replace " + exe + ": " + osCause(err) + ".", unchanged: true, err: err}
	}
	return nil
}

// writeStagedBinary writes binary to path with the executable mode and an fsync, so a crash or
// power loss leaves either no stage or a complete runnable one — never a half-written candidate a
// later resume could execute.
func writeStagedBinary(path string, binary []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return &installFailure{reason: "Could not prepare the new Coop binary: " + osCause(err) + ".", unchanged: true, err: err}
	}
	name := f.Name()
	_, werr := f.Write(binary)
	if cerr := f.Chmod(0o755); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		err := werr
		if err == nil {
			err = cerr
		}
		return &installFailure{reason: "Could not prepare the new Coop binary: " + osCause(err) + ".", unchanged: true, err: err}
	}
	return nil
}

func fetchReleaseFile(tag, name string, limit int64) ([]byte, error) {
	resp, err := releaseHTTPClient.Get(releaseFileURLFor(tag, name))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return data, nil
}

func verifyReleaseChecksum(asset string, archive, checksums []byte) error {
	want := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if want != "" {
			return fmt.Errorf("the release checksums contain more than one entry for %s", asset)
		}
		want = fields[0]
	}
	if len(want) != sha256.Size*2 {
		return fmt.Errorf("the release checksums contain no valid entry for %s", asset)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if !strings.EqualFold(want, got) {
		return errors.New("the downloaded release did not match its published checksum")
	}
	return nil
}

func releaseBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the downloaded release archive could not be read: %w", err)
	}
	defer gz.Close()

	var binary []byte
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the downloaded release archive could not be read: %w", err)
		}
		if hdr.Name != "coop" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, errors.New("the release archive's coop entry is not a regular file")
		}
		if binary != nil {
			return nil, errors.New("the release archive contains more than one Coop binary")
		}
		binary, err = io.ReadAll(io.LimitReader(tr, maxReleaseBinaryBytes+1))
		if err != nil {
			return nil, fmt.Errorf("the downloaded release archive could not be read: %w", err)
		}
		if int64(len(binary)) > maxReleaseBinaryBytes {
			return nil, errors.New("the release archive's Coop binary exceeds the supported size")
		}
	}
	if binary == nil {
		return nil, errors.New("the release archive contains no Coop binary")
	}
	return binary, nil
}
