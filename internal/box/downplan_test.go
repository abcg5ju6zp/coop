package box

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

// downStateShim is a stateful fake docker CLI backed by marker files: containers, networks and
// volumes exist while their markers exist, so one invocation's removals are visible to the next.
// That is what lets a test prove the manifest resumes against actual runtime state.
const downStateShimScript = `#!/bin/sh
STATE=%[1]s
mkdir -p "$STATE/containers" "$STATE/networks" "$STATE/volumes"
while [ $# -gt 0 ]; do
  case "$1" in --config|--host) shift 2 ;; *) break ;; esac
done
cmd="$1"; [ $# -gt 0 ] && shift
for last; do :; done
filters=''
prev=''
for a in "$@"; do
  if [ "$prev" = "--filter" ]; then filters="$filters|$a"; fi
  prev="$a"
done
has_filter() { case "$filters" in *"|$1"*) return 0 ;; *) return 1 ;; esac; }
filter_value() {
  prefix="$1="
  OLDIFS=$IFS; IFS='|'; for f in $filters; do
    case "$f" in "$prefix"*) echo "${f#$prefix}"; IFS=$OLDIFS; return 0 ;; esac
  done; IFS=$OLDIFS
  return 1
}
container_list() {
  label_k=""; label_v=""; net=""; idf=""
  v=$(filter_value label); [ -n "$v" ] && { label_k="${v%%=*}"; label_v="${v#*=}"; }
  net=$(filter_value network)
  idf=$(filter_value id)
  for m in "$STATE/containers/"*; do
    [ -e "$m" ] || continue
    id=$(basename "$m")
    [ -n "$idf" ] && [ "$id" != "$idf" ] && continue
    [ -n "$label_k" ] && ! grep -q "^label $label_k=$label_v$" "$m" && continue
    [ -n "$net" ] && ! grep -q "^network $net$" "$m" && continue
    echo "$id"
  done
}
case "$cmd" in
  info) exit 0 ;;
  inspect)
    id="$last"
    printf '{'
    first=1
    while IFS= read -r line; do
      case "$line" in
        label\ *)
          kv="${line#label }"; k="${kv%%=*}"; v="${kv#*=}"
          [ $first -eq 0 ] && printf ','
          printf '"%s":"%s"' "$k" "$v"; first=0 ;;
      esac
    done < "$STATE/containers/$id"
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
        idf=$(filter_value id)
        v=$(filter_value label)
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
    project=""; action=""
    prev=""
    for a in "$@"; do
      [ "$prev" = "-p" ] && project="$a"
      case "$a" in down|up|config) action="$a" ;; esac
      prev="$a"
    done
    if [ "$action" = "down" ]; then
      echo '[Compose output]'
      [ -f "$STATE/down.says" ] && cat "$STATE/down.says" >&2
      snap=""; pprev=""
      for a in "$@"; do [ "$pprev" = "-f" ] && snap="$a"; pprev="$a"; done
      services=""
      if [ -n "$snap" ] && [ -f "$snap" ]; then
        services=$(awk '/^services:/{f=1;next} /^[A-Za-z]/{f=0} f && /^  [A-Za-z0-9_-]+:/{gsub(/[: ]/,""); print}' "$snap")
      fi
      for m in "$STATE/containers/"*; do
        [ -e "$m" ] || continue
        id=$(basename "$m")
        grep -q "^label com.docker.compose.project=$project$" "$m" || continue
        if [ -f "$STATE/down.keep" ] && grep -qx "$id" "$STATE/down.keep"; then continue; fi
        svc=$(sed -n 's/^label com.docker.compose.service=//p' "$m")
        if [ -n "$services" ] && ! echo "$services" | grep -qxF "$svc"; then continue; fi
        rm -f "$m"
      done
      for n in "$STATE/networks/"*; do
        [ -e "$n" ] || continue
        nid=$(basename "$n"); used=0
        for c in "$STATE/containers/"*; do [ -e "$c" ] && grep -q "^network $nid$" "$c" && used=1 && break; done
        [ $used -eq 0 ] && rm -f "$n" "$STATE/networks-labels/$nid"
      done
      [ -f "$STATE/down.exit" ] && exit "$(cat "$STATE/down.exit")"
      exit 0
    fi ;;
esac
exit 0
`

const (
	downTestDBID      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	downTestRedisID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	downTestNetworkID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type downFixture struct {
	t       *testing.T
	state   string
	repo    string
	project string
	rt      runtime.Runtime
	daemon  func() (dockersock.Info, error)
}

func newDownFixture(t *testing.T) *downFixture {
	t.Helper()
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	state := t.TempDir()
	for _, sub := range []string{"containers", "networks", "networks-labels", "volumes"} {
		if err := os.MkdirAll(filepath.Join(state, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	info := func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", OSType: "linux", Architecture: "amd64", ServerVersion: "29.1", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}
	repo := testComposeRepo(t)
	f := &downFixture{t: t, state: state, repo: repo, project: ComposeProject(repo), daemon: info}
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) { return f.daemon() }))
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(downStateShimScript, "%[1]s", "'"+state+"'")), 0o755); err != nil {
		t.Fatal(err)
	}
	f.rt = runtime.Runtime{Name: path}
	f.addContainer(downTestDBID, "db", false)
	f.addContainer(downTestRedisID, "redis", false)
	f.addNetwork(downTestNetworkID, "default")
	f.addVolume("pgdata", "2026-01-01T00:00:00Z")
	f.addVolume("redisdata", "2026-02-02T00:00:00Z")
	return f
}

func (f *downFixture) addContainer(id, service string, oneoff bool) {
	oneoffFlag := "False"
	if oneoff {
		oneoffFlag = "True"
	}
	body := "label com.docker.compose.project=" + f.project + "\n" +
		"label com.docker.compose.project.working_dir=" + filepath.Join(f.repo, ".agent") + "\n" +
		"label com.docker.compose.service=" + service + "\n" +
		"label com.docker.compose.oneoff=" + oneoffFlag + "\n" +
		"network " + downTestNetworkID + "\n"
	if err := os.WriteFile(filepath.Join(f.state, "containers", id), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *downFixture) addNetwork(id, logical string) {
	name := f.project + "_" + logical
	if err := os.WriteFile(filepath.Join(f.state, "networks", id), []byte(name), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, "networks-labels", id),
		[]byte("com.docker.compose.project="+f.project+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *downFixture) addVolume(short, createdAt string) string {
	name := f.project + "_" + short
	body := createdAt + "\n/var/lib/docker/volumes/" + name + "/_data\n"
	if err := os.WriteFile(filepath.Join(f.state, "volumes", name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return name
}

func (f *downFixture) volumeExists(name string) bool {
	_, err := os.Stat(filepath.Join(f.state, "volumes", name))
	return err == nil
}

func (f *downFixture) targetVolumes() []ServiceVolume {
	return []ServiceVolume{
		{Name: f.project + "_pgdata", Holds: "database data"},
		{Name: f.project + "_redisdata", Holds: "Redis data"},
	}
}

func (f *downFixture) composeFile() string {
	return filepath.Join(f.repo, ".agent", "compose.yml")
}

func (f *downFixture) planPath() string {
	root, err := serviceStateRoot("down-plans")
	if err != nil {
		f.t.Fatal(err)
	}
	return filepath.Join(root, f.project+".json")
}

func (f *downFixture) planExists() bool {
	_, err := os.Stat(f.planPath())
	return err == nil
}

func TestDownPlanPersistsFrozenScopeBeforeAnyRemoval(t *testing.T) {
	f := newDownFixture(t)
	targets := f.targetVolumes()
	// A volume the preview did not name must never enter the frozen manifest.
	f.addVolume("appeared_late", "2026-03-03T00:00:00Z")
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), targets)
	if err != nil {
		t.Fatalf("PrepareDownPlan: %v", err)
	}
	if !f.planExists() {
		t.Fatal("manifest was not persisted before execution")
	}
	if len(plan.Containers) != 2 || len(plan.Networks) != 1 || len(plan.Volumes) != 2 {
		t.Fatalf("frozen scope = %d containers, %d networks, %d volumes, want 2/1/2",
			len(plan.Containers), len(plan.Networks), len(plan.Volumes))
	}
	for _, v := range plan.Volumes {
		if strings.HasSuffix(v.Name, "appeared_late") {
			t.Fatal("a volume unseen at confirmation entered the manifest")
		}
		if v.CreatedAt == "" || v.Mountpoint == "" {
			t.Fatalf("volume %s missing its frozen identity: %+v", v.Name, v)
		}
	}
	for _, r := range plan.Containers {
		if r.Name != "db" && r.Name != "redis" {
			t.Fatalf("container frozen without its service name: %+v", r)
		}
	}
	if string(plan.Compose) == "" || plan.DaemonID != "fixture-daemon" || plan.Endpoint == "" {
		t.Fatalf("manifest missing compose bytes or daemon binding: %+v", plan)
	}
	// Freeze alone removes nothing.
	if !f.volumeExists(f.project + "_pgdata") {
		t.Fatal("a volume disappeared during the freeze")
	}
}

func TestDownPlanFullRunDeletesEverythingAndTheRecord(t *testing.T) {
	f := newDownFixture(t)
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), f.targetVolumes())
	if err != nil {
		t.Fatal(err)
	}
	exec, err := ExecuteDownPlan(context.Background(), f.rt, f.repo, plan, io.Discard, io.Discard)
	if err != nil || exec.DaemonUnreachable || exec.ComposeErr != nil {
		t.Fatalf("ExecuteDownPlan = %+v, %v", exec, err)
	}
	if f.planExists() {
		t.Fatal("completed manifest was not removed")
	}
	if f.volumeExists(f.project+"_pgdata") || f.volumeExists(f.project+"_redisdata") {
		t.Fatal("volumes survive a completed deletion")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.state, "containers")); len(entries) != 0 {
		t.Fatal("containers survive a completed deletion")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.state, "networks")); len(entries) != 0 {
		t.Fatal("networks survive a completed deletion")
	}
}

func TestDownPlanPartialFailureKeepsTodoAndResumeConverges(t *testing.T) {
	f := newDownFixture(t)
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), f.targetVolumes())
	if err != nil {
		t.Fatal(err)
	}
	// Compose fails after removing the db container only; the volume removal stays refused too.
	if err := os.WriteFile(filepath.Join(f.state, "down.exit"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, "down.keep"), []byte(downTestRedisID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, "down.says"), []byte("compose failed mid-teardown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, "volume-rm-fail"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, "container-rm-fail"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec, err := ExecuteDownPlan(context.Background(), f.rt, f.repo, plan, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if exec.ComposeErr == nil {
		t.Fatal("a failed compose down must surface as the phase error")
	}
	if !f.planExists() {
		t.Fatal("a partial run must preserve its manifest")
	}
	if pending := plan.Pending(); len(pending) != 4 { // redis container, network, 2 volumes
		t.Fatalf("pending after partial = %v, want redis container + network + 2 volumes", pending)
	}
	if deleted := plan.Deleted(); len(deleted) != 1 || deleted[0].Name != "db" {
		t.Fatalf("deleted after partial = %v, want only db", plan.Deleted())
	}
	// Recovery: the daemon finishes the teardown on the next SAME command.
	os.Remove(filepath.Join(f.state, "down.exit"))
	os.Remove(filepath.Join(f.state, "down.keep"))
	os.Remove(filepath.Join(f.state, "down.says"))
	os.Remove(filepath.Join(f.state, "volume-rm-fail"))
	os.Remove(filepath.Join(f.state, "container-rm-fail"))
	resumed, err := LoadDownPlan(f.repo)
	if err != nil || resumed == nil {
		t.Fatalf("resume could not load the manifest: %v", err)
	}
	exec, err = ExecuteDownPlan(context.Background(), f.rt, f.repo, resumed, io.Discard, io.Discard)
	if err != nil || exec.DaemonUnreachable || exec.ComposeErr != nil {
		t.Fatalf("resume = %+v, %v", exec, err)
	}
	if f.planExists() {
		t.Fatal("a converged resume must remove the manifest")
	}
	if pending := resumed.Pending(); len(pending) != 0 {
		t.Fatalf("resume left pending: %v", pending)
	}
	if f.volumeExists(f.project+"_pgdata") || f.volumeExists(f.project+"_redisdata") {
		t.Fatal("resume did not finish the volume deletion")
	}
}

func TestDownPlanRecreatedVolumeIsRefusedAndKept(t *testing.T) {
	f := newDownFixture(t)
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), f.targetVolumes())
	if err != nil {
		t.Fatal(err)
	}
	// Between confirmation and execution the redis volume is replaced by different storage.
	victim := f.project + "_redisdata"
	replacer := "2026-09-09T00:00:00Z\n/var/lib/docker/volumes/some-other-daemon/_data\n"
	if err := os.WriteFile(filepath.Join(f.state, "volumes", victim), []byte(replacer), 0o644); err != nil {
		t.Fatal(err)
	}
	exec, err := ExecuteDownPlan(context.Background(), f.rt, f.repo, plan, io.Discard, io.Discard)
	if err != nil || exec.DaemonUnreachable {
		t.Fatalf("execute = %+v, %v", exec, err)
	}
	if !f.volumeExists(victim) {
		t.Fatal("the recreated volume must not be deleted")
	}
	if f.volumeExists(f.project + "_pgdata") {
		t.Fatal("the unchanged volume should have been deleted")
	}
	refused := plan.Refused()
	if len(refused) != 1 || refused[0].Name != victim {
		t.Fatalf("refused = %v, want only the recreated volume", refused)
	}
	if len(plan.Pending()) != 0 {
		t.Fatalf("refusal is terminal; pending = %v", plan.Pending())
	}
	if f.planExists() {
		t.Fatal("a terminal manifest (no pending) must be removed so a fresh confirmation is possible")
	}
}

func TestDownPlanUnreachableDaemonPreservesTheWholeTodo(t *testing.T) {
	f := newDownFixture(t)
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), f.targetVolumes())
	if err != nil {
		t.Fatal(err)
	}
	// Point the recorded endpoint at a socket nobody answers: every bind fails, nothing is
	// observable, so no resource status may move.
	plan.Endpoint = "unix://" + filepath.Join(t.TempDir(), "absent.sock")
	exec, err := ExecuteDownPlan(context.Background(), f.rt, f.repo, plan, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !exec.DaemonUnreachable {
		t.Fatal("an unanswerable daemon must classify the run as unreachable")
	}
	if !f.planExists() {
		t.Fatal("the manifest must survive an unreachable daemon")
	}
	if pending := plan.Pending(); len(pending) != 5 {
		t.Fatalf("pending = %d, want the whole todo (2 containers, 1 network, 2 volumes)", len(pending))
	}
}

func TestDownPlanChangedComposeFileCannotWidenScope(t *testing.T) {
	f := newDownFixture(t)
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), f.targetVolumes())
	if err != nil {
		t.Fatal(err)
	}
	// After confirmation the Compose file declares a new service and a new project volume.
	edited := "services:\n  db:\n    image: postgres:18\n  cache:\n    image: memcached:1\nvolumes:\n  newcache:\n"
	if err := os.WriteFile(f.composeFile(), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	newVolume := f.project + "_newcache"
	f.addVolume("newcache", "2026-04-04T00:00:00Z")
	// And a container the new stack started appears under the same project label, on ITS OWN new
	// network (the frozen network's id must not protect or remove it either).
	cacheID := strings.Repeat("d", 64)
	cacheMarker := "label com.docker.compose.project=" + f.project + "\n" +
		"label com.docker.compose.service=cache\nlabel com.docker.compose.oneoff=False\n" +
		"network " + strings.Repeat("e", 64) + "\n"
	if err := os.WriteFile(filepath.Join(f.state, "containers", cacheID), []byte(cacheMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	exec, err := ExecuteDownPlan(context.Background(), f.rt, f.repo, plan, io.Discard, io.Discard)
	if err != nil || exec.DaemonUnreachable || exec.ComposeErr != nil {
		t.Fatalf("execute = %+v, %v", exec, err)
	}
	if !f.volumeExists(newVolume) {
		t.Fatal("a volume appearing after confirmation was deleted: scope widened")
	}
	// The frozen container set is gone; the new container carrying the project label is not
	// removed by the frozen teardown itself (its cleanup is the next explicit confirmation).
	if _, err := os.Stat(filepath.Join(f.state, "containers", strings.Repeat("d", 64))); err != nil {
		t.Fatal("the frozen teardown removed a container outside its confirmed scope")
	}
	if len(plan.Pending()) != 0 || len(plan.Refused()) != 0 {
		t.Fatalf("frozen resources did not converge: pending=%v refused=%v", plan.Pending(), plan.Refused())
	}
}

func TestPlainDownWritesNoDeletionManifest(t *testing.T) {
	f := newDownFixture(t)
	if err := DownServicesFile(f.rt, f.repo, f.composeFile(), false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	root, err := serviceStateRoot("down-plans")
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("plain down created manifest records: %v", entries)
	}
}

// A rebuilt daemon that answers the frozen endpoint reconciles by object identity: the same
// storage finishes; only a recreated object is refused (covered separately above).
func TestDownPlanReconcilesAgainstReplacedDaemon(t *testing.T) {
	f := newDownFixture(t)
	plan, err := PrepareDownPlan(context.Background(), f.rt, f.repo, f.composeFile(), f.targetVolumes())
	if err != nil {
		t.Fatal(err)
	}
	f.daemon = func() (dockersock.Info, error) {
		return dockersock.Info{ID: "rebuilt-daemon", OSType: "linux", Architecture: "amd64", ServerVersion: "30.0", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}
	exec, err := ExecuteDownPlan(context.Background(), f.rt, f.repo, plan, io.Discard, io.Discard)
	if err != nil || exec.DaemonUnreachable || exec.ComposeErr != nil {
		t.Fatalf("execute on rebuilt daemon = %+v, %v", exec, err)
	}
	if f.volumeExists(f.project+"_pgdata") || f.volumeExists(f.project+"_redisdata") {
		t.Fatal("same-identity volumes should be removable after a daemon rebuild")
	}
	if len(plan.Pending()) != 0 || len(plan.Refused()) != 0 {
		t.Fatalf("rebuilt-daemon reconcile: pending=%v refused=%v", plan.Pending(), plan.Refused())
	}
}
