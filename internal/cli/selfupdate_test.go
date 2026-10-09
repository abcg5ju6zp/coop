package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stub sets *p to v and returns a restore func, so tests can point package vars
// (Version, the URLs, executablePath) at fixtures: `defer stub(&Version, "2.7.2")()`.
func stub[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func TestVersionHelpers(t *testing.T) {
	for name, tc := range map[string]struct {
		current, latest string
		want            releaseRelation
	}{
		"behind":           {"2.7.2", "v2.7.3", releaseBehind},
		"equal":            {"2.7.3", "v2.7.3", releaseEqual},
		"ahead":            {"3.1.0", "v3.0.0", releaseAhead},
		"dev current":      {"dev", "v3.0.0", releaseInvalid},
		"dirty current":    {"3.0.0+dirty", "v3.0.0", releaseInvalid},
		"malformed latest": {"3.0.0", "latest", releaseInvalid},
		"short version":    {"3.0", "3.0.1", releaseInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			if got := compareReleaseVersions(tc.current, tc.latest); got != tc.want {
				t.Errorf("compareReleaseVersions(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

func TestLatestReleaseTag(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3","name":"2.7.3"}`)
		}))
		defer srv.Close()
		defer stub(&githubLatestURL, srv.URL)()
		got, err := latestReleaseTag()
		if err != nil {
			t.Fatal(err)
		}
		if got != "v2.7.3" {
			t.Errorf("tag = %q, want v2.7.3", got)
		}
	})
	t.Run("non-200 errors", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		defer stub(&githubLatestURL, srv.URL)()
		if _, err := latestReleaseTag(); err == nil {
			t.Error("want an error on a non-200 response")
		}
	})
	t.Run("missing tag errors", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{}`)
		}))
		defer srv.Close()
		defer stub(&githubLatestURL, srv.URL)()
		if _, err := latestReleaseTag(); err == nil {
			t.Error("want an error when the response has no tag_name")
		}
	})
}

func TestStageAndCommitRelease(t *testing.T) {
	archive := testReleaseArchive(t, "new binary", "coop")
	asset := fmt.Sprintf("coop_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset)
	var paths []string
	srv := testReleaseServer(t, archive, []byte(sum), &paths)
	defer srv.Close()
	defer stub(&releaseFileURLFor, func(tag, name string) string {
		return srv.URL + "/" + tag + "/" + name
	})()

	dir := t.TempDir()
	exe := filepath.Join(dir, "renamed-coop")
	mustWrite(t, exe, "old binary")
	before, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, ".coop-candidate-9.9.9")
	hash, err := stageReleaseBinary(staged, "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if hash != fmt.Sprintf("%x", sha256.Sum256([]byte("new binary"))) {
		t.Errorf("stage hash = %s, want the staged binary's checksum", hash)
	}
	if got, err := os.ReadFile(staged); err != nil || string(got) != "new binary" {
		t.Fatalf("staged bytes = %q err=%v", got, err)
	}
	if info, err := os.Stat(staged); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode = %v err=%v", info.Mode(), err)
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "old binary" {
		t.Fatalf("staging changed the active executable: %q err=%v", got, err)
	}
	if err := commitStagedBinary(staged, exe); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Errorf("installed bytes = %q", got)
	}
	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("commit reused the old executable inode; want an atomic rename")
	}
	if after.Mode().Perm() != 0o755 {
		t.Errorf("installed mode = %o, want 755", after.Mode().Perm())
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged candidate remains after commit: %v", err)
	}
	wantPaths := []string{"/v9.9.9/checksums.txt", "/v9.9.9/" + asset}
	if fmt.Sprint(paths) != fmt.Sprint(wantPaths) {
		t.Errorf("release requests = %v, want %v", paths, wantPaths)
	}
}

func TestStageReleaseFailuresKeepExecutable(t *testing.T) {
	good := testReleaseArchive(t, "new binary", "coop")
	missingBinary := testReleaseArchive(t, "readme", "README.md")
	asset := fmt.Sprintf("coop_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	for name, tc := range map[string]struct {
		archive   []byte
		checksums func([]byte) []byte
	}{
		"missing checksum":  {good, func([]byte) []byte { return []byte("deadbeef  other.tar.gz\n") }},
		"checksum mismatch": {good, func([]byte) []byte { return []byte(strings.Repeat("0", 64) + "  " + asset + "\n") }},
		"corrupt archive":   {[]byte("not a tarball"), releaseChecksum(asset)},
		"missing binary":    {missingBinary, releaseChecksum(asset)},
	} {
		t.Run(name, func(t *testing.T) {
			srv := testReleaseServer(t, tc.archive, tc.checksums(tc.archive), nil)
			defer srv.Close()
			defer stub(&releaseFileURLFor, func(tag, name string) string {
				return srv.URL + "/" + tag + "/" + name
			})()
			exe := filepath.Join(t.TempDir(), "coop")
			mustWrite(t, exe, "old binary")
			before, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(filepath.Dir(exe), ".coop-candidate-9.9.9")
			if _, err := stageReleaseBinary(staged, "v9.9.9"); err == nil {
				t.Fatal("want a stage failure")
			}
			got, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "old binary" || !os.SameFile(before, after) {
				t.Errorf("failed stage changed executable: bytes=%q same_inode=%v", got, os.SameFile(before, after))
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Errorf("failed stage left a candidate file: %v", err)
			}
		})
	}
}

func TestResolveSelfUpdate(t *testing.T) {
	for name, version := range map[string]string{
		"dev build":       "dev",
		"dirty build":     "2.7.2+dirty",
		"malformed build": "banana",
	} {
		t.Run(name+" is a no-op without network", func(t *testing.T) {
			defer stub(&Version, version)()
			defer stub(&githubLatestURL, "http://127.0.0.1:1/must-not-be-called")()
			result, exe, err := resolveSelfUpdate()
			if result.Outcome != selfUpdateDev || exe != "" || err != nil {
				t.Fatalf("source build: outcome=%v exe=%q err=%v", result.Outcome, exe, err)
			}
		})
	}

	t.Run("already current does not fetch artifacts", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("release artifacts must not be fetched when already current")
			return ""
		})()
		exe := filepath.Join(t.TempDir(), "coop")
		mustWrite(t, exe, "x")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		result, gotExe, err := resolveSelfUpdate()
		if result.Outcome != selfUpdateCurrent || err != nil {
			t.Fatalf("current: outcome=%v err=%v", result.Outcome, err)
		}
		if want, _ := filepath.EvalSymlinks(exe); gotExe != want {
			t.Errorf("exe = %q, want %q", gotExe, want)
		}
	})

	t.Run("ahead build is not downgraded", func(t *testing.T) {
		defer stub(&Version, "3.1.0")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v3.0.0"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("release artifacts must not be fetched for an ahead build")
			return ""
		})()
		exe := filepath.Join(t.TempDir(), "coop")
		mustWrite(t, exe, "ahead")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		result, _, err := resolveSelfUpdate()
		if result.Outcome != selfUpdateAhead || err != nil {
			t.Fatalf("ahead: outcome=%v err=%v", result.Outcome, err)
		}
		if got, err := os.ReadFile(exe); err != nil || string(got) != "ahead" {
			t.Fatalf("ahead executable changed: bytes=%q err=%v", got, err)
		}
		if result.Current != "3.1.0" || result.Latest != "3.0.0" {
			t.Errorf("ahead result = %+v, want both versions named", result)
		}
	})

	t.Run("newer release resolves behind and names the tag without installing", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("the resolver must not download artifacts")
			return ""
		})()
		exe := filepath.Join(t.TempDir(), "coop")
		mustWrite(t, exe, "old")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		result, gotExe, err := resolveSelfUpdate()
		if result.Outcome != selfUpdateBehind || err != nil {
			t.Fatalf("behind: outcome=%v err=%v", result.Outcome, err)
		}
		wantExe, _ := filepath.EvalSymlinks(exe)
		if gotExe != wantExe || result.LatestTag != "v2.7.3" || result.Latest != "2.7.3" {
			t.Errorf("behind result = %+v exe=%q, want tag v2.7.3 and the exe path", result, gotExe)
		}
		if got, err := os.ReadFile(exe); err != nil || string(got) != "old" {
			t.Fatalf("resolver changed the executable: bytes=%q err=%v", got, err)
		}
	})

	t.Run("check failure is soft (checkError)", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		_, _, err := resolveSelfUpdate()
		var ce checkError
		if !errors.As(err, &ce) {
			t.Fatalf("want a checkError (soft), got %v", err)
		}
	})

	t.Run("malformed latest tag is a soft check failure", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"latest"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("invalid release tag must not fetch artifacts")
			return ""
		})()
		_, _, err := resolveSelfUpdate()
		var ce checkError
		if !errors.As(err, &ce) {
			t.Fatalf("want a checkError (soft), got %v", err)
		}
	})
}

func TestParseUpdateFlags(t *testing.T) {
	cases := []struct {
		name             string
		args             []string
		self, box, check bool
		wantErr          bool
	}{
		{"none", nil, false, false, false, false},
		{"self-only", []string{"--self-only"}, true, false, false, false},
		{"box-only", []string{"--box-only"}, false, true, false, false},
		{"check", []string{"--check"}, false, false, true, false},
		{"both is an error", []string{"--self-only", "--box-only"}, false, false, false, true},
		{"check + self-only is an error", []string{"--check", "--self-only"}, false, false, false, true},
		{"unknown flag", []string{"--wat"}, false, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			self, box, check, err := parseUpdateFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if self != c.self || box != c.box || check != c.check {
				t.Errorf("self=%v box=%v check=%v, want %v/%v/%v", self, box, check, c.self, c.box, c.check)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func testReleaseArchive(t *testing.T, content, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func releaseChecksum(asset string) func([]byte) []byte {
	return func(archive []byte) []byte {
		return []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset))
	}
}

func testReleaseServer(t *testing.T, archive, checksums []byte, paths *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if paths != nil {
			*paths = append(*paths, r.URL.Path)
		}
		switch filepath.Base(r.URL.Path) {
		case "checksums.txt":
			_, _ = w.Write(checksums)
		case fmt.Sprintf("coop_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
			fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
}

// A wrong or hostile asset must fail at the size cap, never be read whole into memory first.
func TestFetchReleaseFileRejectsOversizedAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 17))
	}))
	defer srv.Close()
	defer stub(&releaseFileURLFor, func(tag, name string) string { return srv.URL + "/" + tag + "/" + name })()
	if _, err := fetchReleaseFile("v1.0.0", "big.bin", 16); err == nil || !strings.Contains(err.Error(), "exceeds 16 bytes") {
		t.Fatalf("oversized asset error = %v, want the cap named", err)
	}
	if data, err := fetchReleaseFile("v1.0.0", "fits.bin", 17); err != nil || len(data) != 17 {
		t.Fatalf("asset at the cap = %d bytes, %v; want it accepted", len(data), err)
	}
}
