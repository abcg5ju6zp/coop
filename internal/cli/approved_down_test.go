package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

// The stateful transcripts for `coop down --delete-volumes`: unlike composeShim, this fake keeps
// its containers, networks and volumes as marker files, so a second invocation observes exactly
// what the first one removed — the property the persisted manifest exists to make resumable.
const downShimScript = `#!/bin/sh
STATE=%[1]s
mkdir -p "$STATE/containers" "$STATE/networks" "$STATE/networks-labels" "$STATE/volumes"
while [ $# -gt 0 ]; do
  case "$1" in --config|--host) shift 2 ;; *) break ;; esac
done
cmd="$1"; [ $# -gt 0 ] && shift
for last; do :; done
filters=''; prev=''
for a in "$@"; do
  if [ "$prev" = "--filter" ]; then filters="$filters|$a"; fi
  prev="$a"
done
filter_value() {
  prefix="$1="
  OLDIFS=$IFS; IFS='|'; for f in $filters; do
    case "$f" in "$prefix"*) echo "${f#$prefix}"; IFS=$OLDIFS; return 0 ;; esac
  done; IFS=$OLDIFS
  return 1
}
container_list() {
  lk=''; lv=''; net=''; idf=''
  v=$(filter_value label); [ -n "$v" ] && { lk="${v%%=*}"; lv="${v#*=}"; }
  net=$(filter_value network); idf=$(filter_value id)
  for m in "$STATE/containers/"*; do
    [ -e "$m" ] || continue
    id=$(basename "$m")
    [ -n "$idf" ] && [ "$id" != "$idf" ] && continue
    [ -n "$lk" ] && ! grep -q "^label $lk=$lv$" "$m" && continue
    [ -n "$net" ] && ! grep -q "^network $net$" "$m" && continue
    echo "$id"
  done
}
case "$cmd" in
  info) exit 0 ;;
  inspect)
    printf '{'
    first=1
    while IFS= read -r line; do
      case "$line" in
        label\ *)
          kv="${line#label }"; k="${kv%%=*}"; v="${kv#*=}"
          [ $first -eq 0 ] && printf ','
          printf '"%s":"%s"' "$k" "$v"; first=0 ;;
      esac
    done < "$STATE/containers/$last"
    printf '}\n' ;;
  container)
    sub="$1"; shift
    case "$sub" in
      ls) container_list ;;
      rm)
        for a in "$@"; do
          [ "$a" = "--force" ] && continue
          if [ -f "$STATE/container-rm-fail" ]; then echo "container removal already in progress" >&2; exit 1; fi
          rm -f "$STATE/containers/$a"
        done ;;
    esac ;;
  network)
    sub="$1"; shift
    case "$sub" in
      ls)
        idf=$(filter_value id); v=$(filter_value label)
        for m in "$STATE/networks/"*; do
          [ -e "$m" ] || continue
          id=$(basename "$m")
          [ -n "$idf" ] && [ "$id" != "$idf" ] && continue
          [ -n "$v" ] && { k="${v%%=*}"; val="${v#*=}"; grep -q "^$k=$val$" "$STATE/networks-labels/$id" 2>/dev/null || continue; }
          echo "$id"
        done ;;
      inspect) cat "$STATE/networks/$last" 2>/dev/null || exit 1 ;;
      rm)
        id="$last"; used=0
        for c in "$STATE/containers/"*; do [ -e "$c" ] && grep -q "^network $id$" "$c" && used=1 && break; done
        if [ $used -eq 1 ]; then echo "network has active endpoints" >&2; exit 1; fi
        rm -f "$STATE/networks/$id" "$STATE/networks-labels/$id" ;;
    esac ;;
  volume)
    sub="$1"; shift
    case "$sub" in
      ls)
        if name=$(filter_value name); then
          [ -f "$STATE/volumes/$name" ] && printf '"%s"\n' "$name"
        else
          for m in "$STATE/volumes/"*; do [ -e "$m" ] && basename "$m"; done
        fi ;;
      inspect)
        f="$STATE/volumes/$last"
        if [ ! -f "$f" ]; then echo "No such volume" >&2; exit 1; fi
        ca=$(sed -n 1p "$f"); mp=$(sed -n 2p "$f")
        printf '{"Name":"%s","Driver":"local","Scope":"local","Mountpoint":"%s","CreatedAt":"%s","Options":{}}\n' "$last" "$mp" "$ca" ;;
      rm)
        for name in "$@"; do
          if [ -f "$STATE/volumes/$name" ] && [ -f "$STATE/volume-rm-fail" ]; then
            echo "volume is in use" >&2; exit 1
          fi
          rm -f "$STATE/volumes/$name"
        done ;;
    esac ;;
  compose)
    project=''; action=''; snap=''; prev=''; pprev=''
    for a in "$@"; do
      [ "$prev" = "-p" ] && project="$a"
      [ "$pprev" = "-f" ] && snap="$a"
      case "$a" in down|up|config) action="$a" ;; esac
      pprev="$prev"; prev="$a"
    done
    if [ "$action" = "down" ]; then
      echo '[Compose output]'
      [ -f "$STATE/down.says" ] && cat "$STATE/down.says" >&2
      services=''
      [ -f "$snap" ] && services=$(awk '/^services:/{f=1;next} /^[A-Za-z]/{f=0} f && /^  [A-Za-z0-9_-]+:/{gsub(/[: ]/,""); print}' "$snap")
      for m in "$STATE/containers/"*; do
        [ -e "$m" ] || continue
        id=$(basename "$m")
        grep -q "^label com.docker.compose.project=$project$" "$m" || continue
        [ -f "$STATE/down.keep" ] && grep -qx "$id" "$STATE/down.keep" && continue
        svc=$(sed -n 's/^label com.docker.compose.service=//p' "$m")
        [ -n "$services" ] && ! echo "$services" | grep -qxF "$svc" && continue
        rm -f "$m"
      done
      for n in "$STATE/networks/"*; do
        [ -e "$n" ] || continue
        nid=$(basename "$n"); used=0
        for c in "$STATE/containers/"*; do [ -e "$c" ] && grep -q "^network $nid$" "$c" && used=1 && break; done
        [ $used -eq 0 ] && rm -f "$n" "$STATE/networks-labels/$nid"
      done
      [ -f "$STATE/down.exit" ] && exit "$(cat "$STATE/down.exit")"
    fi ;;
esac
exit 0
`

const (
	downShimDBID      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	downShimRedisID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	downShimNetworkID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type downShim struct {
	t      *testing.T
	state  string
	repo   string
	daemon func() (dockersock.Info, error)
	rt     runtime.Runtime
}

func newDownShim(t *testing.T) *downShim {
	t.Helper()
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	state := t.TempDir()
	for _, sub := range []string{"containers", "networks", "networks-labels", "volumes"} {
		if err := os.MkdirAll(filepath.Join(state, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	daemon := func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", OSType: "linux", Architecture: "amd64", ServerVersion: "29.1", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}
	repo := serviceProject(t)
	s := &downShim{t: t, state: state, repo: repo, daemon: daemon}
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) { return s.daemon() }))
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(downShimScript, "%[1]s", "'"+state+"'")), 0o755); err != nil {
		t.Fatal(err)
	}
	s.rt = runtime.Runtime{Name: path}
	s.seed()
	return s
}

func (s *downShim) seed() {
	project := box.ComposeProject(s.repo)
	s.container(downShimDBID, project, "db")
	s.container(downShimRedisID, project, "redis")
	networkName := project + "_default"
	s.write(filepath.Join("networks", downShimNetworkID), networkName)
	s.write(filepath.Join("networks-labels", downShimNetworkID), "com.docker.compose.project="+project)
	for _, short := range []string{"pgdata", "redisdata"} {
		name := project + "_" + short
		s.write(filepath.Join("volumes", name), "2026-01-01T00:00:00Z\n/var/lib/docker/volumes/"+name+"/_data\n")
	}
}

func (s *downShim) recreateVolume(short string) {
	name := box.ComposeProject(s.repo) + "_" + short
	s.write(filepath.Join("volumes", name), "2026-09-09T00:00:00Z\n/var/lib/docker/volumes/some-other-daemon/_data\n")
}

func (s *downShim) volumeExists(short string) bool {
	name := box.ComposeProject(s.repo) + "_" + short
	_, err := os.Stat(filepath.Join(s.state, "volumes", name))
	return err == nil
}

func (s *downShim) container(id, project, service string) {
	body := "label com.docker.compose.project=" + project + "\n" +
		"label com.docker.compose.service=" + service + "\n" +
		"label com.docker.compose.oneoff=False\nnetwork " + downShimNetworkID + "\n"
	s.write(filepath.Join("containers", id), body)
}

func (s *downShim) write(rel, body string) {
	if err := os.WriteFile(filepath.Join(s.state, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *downShim) flag(name string) { s.write(name, "1") }

func (s *downShim) failTeardown() {
	s.flag("down.exit")
	s.write("down.keep", downShimDBID+"\n"+downShimRedisID+"\n")
	s.write("down.says", "compose failed mid-teardown\n")
	s.flag("container-rm-fail")
	s.flag("volume-rm-fail")
}

func (s *downShim) recover() {
	for _, name := range []string{"down.exit", "down.keep", "down.says", "container-rm-fail", "volume-rm-fail"} {
		_ = os.Remove(filepath.Join(s.state, name))
	}
}

func (s *downShim) app() *app {
	return &app{cfg: &config.Config{RepoOverride: s.repo}, rt: s.rt, rtSet: true}
}

func TestApprovedDownDeletion(t *testing.T) {
	// The preview, then the shared gate, then the stop — in that order, so the answer is given
	// against the list and nothing is removed before it.
	t.Run("29d-delete-volumes-accepted", func(t *testing.T) {
		s := newDownShim(t)
		a := s.app()
		typedAnswers(t, "y")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 0 {
			t.Fatalf("an accepted deletion exited %d:\n%s", code, out)
		}
		assertApprovedSession(t, "29d-delete-volumes-accepted", normalizeProject(t, s.repo, out))
	})

	t.Run("29g-delete-volumes-yes", func(t *testing.T) {
		s := newDownShim(t)
		a := s.app()
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if code != 0 {
			t.Fatalf("a confirmed deletion exited %d:\n%s", code, out)
		}
		assertApprovedSession(t, "29g-delete-volumes-yes", normalizeProject(t, s.repo, out))
	})

	// Compose fails before removing anything; nothing is deleted and the whole frozen todo stays.
	t.Run("29k-volume-deletion-failed", func(t *testing.T) {
		s := newDownShim(t)
		s.failTeardown()
		a := s.app()
		typedAnswers(t, "y")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 1 {
			t.Errorf("a failed deletion exited %d, want 1", code)
		}
		assertApprovedSession(t, "29k-volume-deletion-failed", normalizeProject(t, s.repo, out))
	})

	// The next same command resumes without asking again and finishes the frozen todo.
	t.Run("29l-delete-volumes-resumed", func(t *testing.T) {
		s := newDownShim(t)
		s.failTeardown()
		a := s.app()
		typedAnswers(t, "y")
		var firstCode int
		first := captureTerminal(t, func() { firstCode, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if firstCode != 1 || !strings.Contains(first, "Pending") {
			t.Fatalf("first call should leave a pending deletion, got %d:\n%s", firstCode, first)
		}
		s.recover()
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 0 {
			t.Fatalf("the resumed deletion exited %d:\n%s", code, out)
		}
		if s.volumeExists("pgdata") || s.volumeExists("redisdata") {
			t.Fatal("resumed deletion left volumes behind")
		}
		assertApprovedSession(t, "29l-delete-volumes-resumed", normalizeProject(t, s.repo, out))
	})

	// A volume recreated by different storage under the same name is refused, not deleted; the
	// rest finishes and the refused name waits for a fresh explicit confirmation.
	t.Run("29m-delete-volumes-refused-identity", func(t *testing.T) {
		s := newDownShim(t)
		s.failTeardown()
		a := s.app()
		var firstCode int
		_ = captureTerminal(t, func() { firstCode, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if firstCode != 1 {
			t.Fatalf("first call should fail the teardown, got %d", firstCode)
		}
		s.recover()
		s.recreateVolume("redisdata")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if code != 1 {
			t.Errorf("an identity refusal exits 1, got %d", code)
		}
		if !s.volumeExists("redisdata") {
			t.Fatal("the recreated volume was deleted")
		}
		if s.volumeExists("pgdata") {
			t.Fatal("the unchanged volume was not deleted")
		}
		assertApprovedSession(t, "29m-delete-volumes-refused-identity", normalizeProject(t, s.repo, out))
	})

	// With the daemon down at resume, the whole todo is preserved and the retry instruction names
	// the same command.
	t.Run("29n-delete-volumes-daemon-unreachable", func(t *testing.T) {
		s := newDownShim(t)
		s.failTeardown()
		a := s.app()
		var firstCode int
		_ = captureTerminal(t, func() { firstCode, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if firstCode != 1 {
			t.Fatalf("first call should fail the teardown, got %d", firstCode)
		}
		s.daemon = func() (dockersock.Info, error) { return dockersock.Info{}, dockersock.ErrUnavailable }
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if code != 1 {
			t.Errorf("an unreachable daemon exits 1, got %d", code)
		}
		assertApprovedSession(t, "29n-delete-volumes-daemon-unreachable", normalizeProject(t, s.repo, out))
	})

	// A plain stop while a confirmed deletion is still pending names the pending work, keeps the
	// data, and never touches the volume manifest.
	t.Run("29o-plain-down-with-pending-plan", func(t *testing.T) {
		s := newDownShim(t)
		s.failTeardown()
		a := s.app()
		var firstCode int
		_ = captureTerminal(t, func() { firstCode, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if firstCode != 1 {
			t.Fatalf("first call should fail the teardown, got %d", firstCode)
		}
		s.recover()
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown(nil) })
		if code != 0 {
			t.Fatalf("plain down exited %d:\n%s", code, out)
		}
		if !s.volumeExists("pgdata") || !s.volumeExists("redisdata") {
			t.Fatal("a plain stop must keep the pending deletion's volumes")
		}
		assertApprovedSession(t, "29o-plain-down-with-pending-plan", normalizeProject(t, s.repo, out))
	})
}
