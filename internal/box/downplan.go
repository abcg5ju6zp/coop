package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// A confirmed `coop down --delete-volumes` is a two-phase, resumable operation. PrepareDownPlan
// freezes the exact daemon, containers, networks, volumes (with object identities) and Compose
// bytes the human confirmed and writes them to host state BEFORE anything is removed.
// ExecuteDownPlan then removes only what the manifest names, marks a resource deleted only after
// the runtime confirms it disappeared, and persists that progress per resource — so a Compose
// partial failure, an unreachable daemon, or an interrupted process resumes in the next same
// command, and a volume name reused by a different object is refused instead of deleted.

const downPlanVersion = 1

type downStatus string

const (
	downPending downStatus = "pending"
	downDeleted downStatus = "deleted"
	downRefused downStatus = "refused"
)

// DownPlannedResource is one frozen container or network: identified by the runtime's immutable
// id (the display Name is best-effort labelling, never used for selection).
type DownPlannedResource struct {
	ID     string     `json:"id"`
	Name   string     `json:"name,omitempty"`
	Status downStatus `json:"status"`
	Reason string     `json:"reason,omitempty"`
}

// DownPlannedVolume is one frozen named volume: the immutable object identity Compose attaches,
// captured behind the name a human approved.
type DownPlannedVolume struct {
	Name       string     `json:"name"`
	Holds      string     `json:"holds,omitempty"`
	CreatedAt  string     `json:"created_at"`
	Mountpoint string     `json:"mountpoint"`
	Status     downStatus `json:"status"`
	Reason     string     `json:"reason,omitempty"`
}

// DownPlan is the persisted deletion manifest for one Compose project.
type DownPlan struct {
	Version     int                   `json:"version"`
	Project     string                `json:"project"`
	Workspace   string                `json:"workspace"`
	File        string                `json:"file"`
	Digest      string                `json:"digest"`
	Endpoint    string                `json:"endpoint,omitempty"`
	DaemonID    string                `json:"daemon_id,omitempty"`
	ConfirmedAt time.Time             `json:"confirmed_at"`
	Compose     []byte                `json:"compose"`
	Containers  []DownPlannedResource `json:"containers"`
	Networks    []DownPlannedResource `json:"networks"`
	Volumes     []DownPlannedVolume   `json:"volumes"`
}

// DownPlanItem is one resource the command reports on, in human terms.
type DownPlanItem struct {
	Kind string // "container", "network", or "volume"
	Name string
}

const (
	downKindContainer = "container"
	downKindNetwork   = "network"
	downKindVolume    = "volume"
)

// Deleted/Pending/Refused project the manifest into what the command's output must distinguish.
// Order is containers, networks, volumes — the order the resources stop in.
func (p *DownPlan) items(status downStatus) []DownPlanItem {
	var items []DownPlanItem
	for _, r := range p.Containers {
		if r.Status == status {
			items = append(items, DownPlanItem{Kind: downKindContainer, Name: downDisplayName(r.Name, r.ID)})
		}
	}
	for _, r := range p.Networks {
		if r.Status == status {
			items = append(items, DownPlanItem{Kind: downKindNetwork, Name: downDisplayName(r.Name, r.ID)})
		}
	}
	for _, v := range p.Volumes {
		if v.Status == status {
			items = append(items, DownPlanItem{Kind: downKindVolume, Name: v.Name})
		}
	}
	return items
}

func downDisplayName(name, id string) string {
	if name != "" {
		return name
	}
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

func (p *DownPlan) Deleted() []DownPlanItem { return p.items(downDeleted) }
func (p *DownPlan) Pending() []DownPlanItem { return p.items(downPending) }
func (p *DownPlan) Refused() []DownPlanItem { return p.items(downRefused) }

// downPlanRoot is host-owned state beside the service approvals: no box or repo can rewrite a
// manifest between confirmation and execution.
func downPlanRoot() (string, error) { return serviceStateRoot("down-plans") }

func downPlanStorePath(workspace string) (string, error) {
	root, err := downPlanRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ComposeProject(workspace)+".json"), nil
}

// LoadDownPlan returns the persisted manifest for this workspace. A missing file is (nil, nil);
// a malformed or foreign record is an error — it blocks a permanent deletion, so coop must not
// silently overwrite or ignore it.
func LoadDownPlan(workspace string) (*DownPlan, error) {
	path, err := downPlanStorePath(workspace)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plan DownPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("the saved volume-deletion record %s is unreadable — inspect or remove it before running coop down: %w", path, err)
	}
	if err := plan.validate(workspace); err != nil {
		return nil, fmt.Errorf("the saved volume-deletion record %s is invalid — inspect or remove it: %w", path, err)
	}
	return &plan, nil
}

func (p *DownPlan) validate(workspace string) error {
	if p.Version != downPlanVersion || p.Project == "" || p.File == "" || p.Digest == "" ||
		len(p.Compose) == 0 || p.Endpoint == "" || p.DaemonID == "" || p.ConfirmedAt.IsZero() {
		return errors.New("missing required fields")
	}
	if p.Project != ComposeProject(workspace) {
		return errors.New("record belongs to a different workspace")
	}
	if p.Workspace != canonicalWorkspace(workspace) {
		return errors.New("record workspace does not match this checkout")
	}
	if p.Digest != composeDigest(p.Compose) {
		return errors.New("recorded Compose bytes fail their digest")
	}
	for _, v := range p.Volumes {
		if v.Name == "" || v.CreatedAt == "" || v.Mountpoint == "" {
			return errors.New("a recorded volume is missing its identity")
		}
	}
	return nil
}

// save writes the manifest atomically with private permissions.
func (p *DownPlan) save() error {
	root, err := downPlanRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(root, p.Project+".json")
	tmp, err := os.CreateTemp(root, "down-plan-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), final)
}

func (p *DownPlan) remove() error {
	path, err := downPlanStorePath(p.Workspace)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return nil
}

const (
	composeNetworkLabel = "com.docker.compose.network"
	refuseVolumeChanged = "volume identity changed after confirmation"
	refuseVolumeStorage = "volume storage driver changed after confirmation"
)

// PrepareDownPlan freezes the confirmed deletion scope and persists every resource as pending
// before returning. targets are exactly the names the human approved; a name that vanished
// between the preview and the freeze is skipped, a name that appeared in between is never added.
func PrepareDownPlan(ctx context.Context, rt runtime.Runtime, workspace, file string, targets []ServiceVolume) (*DownPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	canonical, rel, err := serviceComposePath(workspace, file)
	if err != nil {
		return nil, err
	}
	data, err := readValidatedCompose(file, workspace, false)
	if err != nil {
		return nil, &ComposeRefused{Verb: "stop", File: filepath.Base(file), Err: err}
	}
	bound, err := rt.FreezeCompose(ctx)
	if err != nil {
		return nil, fmt.Errorf("bind the service teardown to Docker: %w", err)
	}
	endpoint, daemonID := bound.ComposeBinding()
	if endpoint == "" || daemonID == "" {
		return nil, errors.New("Docker daemon must be frozen before a volume deletion")
	}
	docker, err := runtime.BindDocker(ctx, bound, endpoint, daemonID)
	if err != nil {
		return nil, fmt.Errorf("bind the service teardown to Docker: %w", err)
	}
	defer docker.Close()
	projectName := ComposeProject(workspace)
	observed, err := docker.LabeledContainers(ctx, composeProjectLabel, projectName)
	if err != nil {
		return nil, fmt.Errorf("list the project's containers: %w", err)
	}
	containers := make([]DownPlannedResource, 0, len(observed))
	for _, c := range observed {
		name := c.Labels[composeServiceLabel]
		if c.Labels[composeOneoffLabel] == "True" {
			name = "one-off " + name
		}
		containers = append(containers, DownPlannedResource{ID: c.ID, Name: name, Status: downPending})
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	networkIDs, err := docker.LabeledNetworkIDs(ctx, composeProjectLabel, projectName)
	if err != nil {
		return nil, fmt.Errorf("list the project's networks: %w", err)
	}
	sort.Strings(networkIDs)
	networks := make([]DownPlannedResource, 0, len(networkIDs))
	for _, id := range networkIDs {
		name, labelErr := docker.NetworkLabelValue(ctx, id, composeNetworkLabel)
		if labelErr != nil {
			return nil, fmt.Errorf("inspect the project's networks: %w", labelErr)
		}
		networks = append(networks, DownPlannedResource{ID: id, Name: name, Status: downPending})
	}
	// The frozen daemon — not the ambient enumeration the preview used — decides which confirmed
	// names are still this project's volumes right now.
	current, err := docker.LabeledVolumeNames(ctx, composeProjectLabel, projectName)
	if err != nil {
		return nil, fmt.Errorf("list the project's volumes: %w: %w", ErrVolumesUnknown, err)
	}
	holds := make(map[string]string, len(targets))
	for _, t := range targets {
		holds[t.Name] = t.Holds
	}
	volumes := make([]DownPlannedVolume, 0, len(targets))
	for _, name := range current {
		if _, confirmed := holds[name]; !confirmed {
			// A volume the human never saw (added to the Compose file after the preview) waits
			// for the next explicit confirmation.
			continue
		}
		identity, present, err := docker.InspectNamedVolume(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("inspect Docker volume %q before deletion: %w", name, err)
		}
		if !present {
			continue
		}
		volumes = append(volumes, DownPlannedVolume{
			Name: name, Holds: holds[name], CreatedAt: identity.CreatedAt,
			Mountpoint: identity.Mountpoint, Status: downPending,
		})
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	plan := &DownPlan{
		Version: downPlanVersion, Project: projectName, Workspace: canonical, File: rel,
		Digest: composeDigest(data), Endpoint: endpoint, DaemonID: daemonID,
		ConfirmedAt: time.Now().UTC(), Compose: data,
		Containers: containers, Networks: networks, Volumes: volumes,
	}
	if err := plan.save(); err != nil {
		return nil, fmt.Errorf("save the volume-deletion record before removing anything: %w", err)
	}
	return plan, nil
}

// DownPlanExecution is what one ExecuteDownPlan pass observed, for the command's own prose.
type DownPlanExecution struct {
	DaemonUnreachable bool
	ComposeErr        error
}

// ExecuteDownPlan performs exactly the persisted manifest against the frozen daemon and records
// progress in the manifest itself. Reconciliation is always by observed reality: gone is done,
// present stays pending, a present volume with a different object identity is refused. When
// nothing remains pending the manifest is deleted; refused resources are terminal and left for a
// fresh explicit confirmation.
func ExecuteDownPlan(ctx context.Context, rt runtime.Runtime, workspace string, plan *DownPlan, stdout, stderr io.Writer, exposedRoots ...string) (*DownPlanExecution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := plan.validate(workspace); err != nil {
		return nil, err
	}
	exec := &DownPlanExecution{}
	docker, err := runtime.BindDocker(ctx, rt, plan.Endpoint, plan.DaemonID)
	if errors.Is(err, runtime.ErrDaemonIdentityChanged) {
		// Another daemon now answers the frozen endpoint. Proceed read-mostly and let each
		// volume's object inspection decide; containers/networks reconcile by immutable id.
		docker, err = runtime.RecoverDocker(ctx, rt, plan.Endpoint)
	}
	if err != nil || docker == nil {
		// Nothing observable means nothing changes: the whole todo stays as it was confirmed.
		exec.DaemonUnreachable = true
		return exec, nil
	}
	defer docker.Close()

	// Compose replays against whoever currently answers the frozen endpoint (its removal is
	// constrained to the frozen file + frozen ids); direct object calls use the same binding.
	if err := executeDownContainers(ctx, rt.ForCompose(docker.Endpoint(), docker.Info().ID), docker, workspace, plan, stdout, stderr, exec, exposedRoots...); err != nil {
		return nil, err
	}
	if err := executeDownNetworks(ctx, docker, plan); err != nil {
		exec.DaemonUnreachable = true
		return exec, nil
	}
	executeDownVolumes(ctx, docker, plan)

	if len(plan.Pending()) == 0 {
		if err := plan.remove(); err != nil {
			return nil, err
		}
	}
	return exec, nil
}

// executeDownContainers reconciles the frozen container ids against reality, and only while
// confirmed containers remain runs Compose down against the FROZEN file bytes and without
// --remove-orphans: a Compose edit after confirmation can neither add nor replace a target.
func executeDownContainers(ctx context.Context, rt runtime.Runtime, docker *runtime.Docker, workspace string, plan *DownPlan, stdout, stderr io.Writer, exec *DownPlanExecution, exposedRoots ...string) error {
	present, err := docker.LabeledContainers(ctx, composeProjectLabel, plan.Project)
	if err != nil {
		exec.DaemonUnreachable = true
		return nil
	}
	presentIDs := make(map[string]bool, len(present))
	for _, c := range present {
		presentIDs[c.ID] = true
	}
	changed := markAbsentContainers(plan, presentIDs)
	pending := pendingContainers(plan)
	if len(pending) == 0 {
		if changed {
			return plan.save()
		}
		return nil
	}
	file := filepath.Join(workspace, filepath.FromSlash(plan.File))
	args, cleanup, snapErr := snapshotFrozenComposeArgs(workspace, file, plan.Project, plan.Compose, exposedRoots...)
	if snapErr != nil {
		return &ComposeRefused{Verb: "stop", File: filepath.Base(file), Err: snapErr}
	}
	defer cleanup()
	boundCompose := rt // already pinned to whichever daemon answered the frozen endpoint
	composeErr := runCompose(boundCompose, stdout, stderr, "down", append(args, "down"))
	if composeErr != nil {
		exec.ComposeErr = composeErr
	}
	// Re-observe: Compose partial failure is reconciled per frozen id, never assumed.
	again, err := docker.LabeledContainers(ctx, composeProjectLabel, plan.Project)
	if err != nil {
		exec.DaemonUnreachable = true
		return nil
	}
	againIDs := make(map[string]bool, len(again))
	for _, c := range again {
		againIDs[c.ID] = true
	}
	for i := range plan.Containers {
		r := &plan.Containers[i]
		if r.Status != downPending {
			continue
		}
		if !againIDs[r.ID] {
			r.Status = downDeleted
			changed = true
			continue
		}
		// Exact-id safety net for a Compose failure and for one-off containers not in the file.
		// New containers carrying the project label are NOT frozen ids, so this never widens.
		if rmErr := docker.RemoveContainerID(ctx, r.ID); rmErr == nil {
			r.Status = downDeleted
			changed = true
		}
	}
	if changed {
		return plan.save()
	}
	return nil
}

// markAbsentContainers records frozen ids the runtime already no longer shows.
func markAbsentContainers(plan *DownPlan, present map[string]bool) bool {
	changed := false
	for i := range plan.Containers {
		if plan.Containers[i].Status == downPending && !present[plan.Containers[i].ID] {
			plan.Containers[i].Status = downDeleted
			changed = true
		}
	}
	return changed
}

func pendingContainers(plan *DownPlan) []DownPlannedResource {
	var out []DownPlannedResource
	for _, r := range plan.Containers {
		if r.Status == downPending {
			out = append(out, r)
		}
	}
	return out
}

func executeDownNetworks(ctx context.Context, docker *runtime.Docker, plan *DownPlan) error {
	current, err := docker.LabeledNetworkIDs(ctx, composeProjectLabel, plan.Project)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(current))
	for _, id := range current {
		present[id] = true
	}
	changed := false
	for i := range plan.Networks {
		r := &plan.Networks[i]
		if r.Status != downPending {
			continue
		}
		if !present[r.ID] {
			r.Status = downDeleted
			changed = true
			continue
		}
		inUse, err := docker.NetworkInUse(ctx, r.ID)
		if err != nil {
			return err
		}
		if inUse {
			continue // a container — possibly one outside the frozen scope — still holds it
		}
		if err := docker.RemoveNetworkID(ctx, r.ID); err == nil {
			r.Status = downDeleted
			changed = true
		}
	}
	if changed {
		return plan.save()
	}
	return nil
}

func executeDownVolumes(ctx context.Context, docker *runtime.Docker, plan *DownPlan) {
	for i := range plan.Volumes {
		v := &plan.Volumes[i]
		if v.Status != downPending {
			continue
		}
		identity, present, err := docker.InspectNamedVolume(ctx, v.Name)
		switch {
		case err != nil && errors.Is(err, runtime.ErrVolumeNotPlainLocal):
			v.Status, v.Reason = downRefused, refuseVolumeStorage
			_ = plan.save()
			continue
		case err != nil:
			continue // outcome unknown; the whole volume stays pending
		case !present:
			// A previous partial run removed it; positive absence is the completion proof.
			v.Status = downDeleted
			_ = plan.save()
			continue
		}
		if identity.CreatedAt != v.CreatedAt || identity.Mountpoint != v.Mountpoint {
			// Different storage under the same name — a recreated volume or another daemon's.
			v.Status, v.Reason = downRefused, refuseVolumeChanged
			_ = plan.save()
			continue
		}
		switch err := docker.RemoveConfirmedVolume(ctx, v.Name); {
		case err == nil:
			v.Status = downDeleted
			_ = plan.save()
		case errors.Is(err, runtime.ErrDaemonIdentityChanged):
			return // the daemon moved mid-phase; leave the remainder untouched for resume
		default:
			// Still present or the outcome is unknown: stay pending for the next same command.
		}
	}
}

// snapshotFrozenComposeArgs builds Compose teardown args from bytes the manifest froze at
// confirmation, written into the same kind of private outside-workspace directory a live
// snapshot uses. It deliberately carries no --remove-orphans at the call site and reads no
// workspace file, so a post-confirmation edit cannot widen the removal.
func snapshotFrozenComposeArgs(workspace, file, projectName string, data []byte, exposedRoots ...string) ([]string, func(), error) {
	dir, err := privateWorkspaceTempDir(workspace, "coop-compose-", exposedRoots...)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		cleanup()
		return nil, nil, err
	}
	projectDir, _ := filepath.Abs(filepath.Dir(file))
	return []string{"compose", "-p", projectName,
		"--project-directory", projectDir, "--env-file", os.DevNull, "-f", path}, cleanup, nil
}

// PendingDownPlan reports whether this workspace has a persisted, unfinished volume deletion —
// a plain `coop down` mentions it without touching it.
func PendingDownPlan(workspace string) bool {
	plan, err := LoadDownPlan(workspace)
	return err == nil && plan != nil && len(plan.Volumes) > 0
}

// PendingDownVolumes is the frozen volume list still awaiting deletion — what a resumed
// invocation prints without asking again.
func PendingDownVolumes(plan *DownPlan) []ServiceVolume {
	out := make([]ServiceVolume, 0, len(plan.Volumes))
	for _, v := range plan.Volumes {
		if v.Status == downPending {
			out = append(out, ServiceVolume{Name: v.Name, Holds: v.Holds})
		}
	}
	slices.SortFunc(out, func(a, b ServiceVolume) int { return strings.Compare(a.Name, b.Name) })
	return out
}
