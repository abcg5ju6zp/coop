package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// This file holds the bound-daemon primitives behind `coop down --delete-volumes`: every query
// and every removal goes to the one frozen endpoint through the private client config, so the
// operation never follows a changed Docker context, and every removal is proven by a follow-up
// observation rather than inferred from a zero exit.

// downLabel validates the one exact-label selector the teardown inventory uses: compose project
// labels, whose key and value are never operator text.
func downLabel(key, value string) error {
	if !dockerToken(key, 128) || strings.Contains(key, "=") || !dockerToken(value, 512) {
		return errors.New("invalid Docker label selector")
	}
	return nil
}

// LabeledContainers lists every running or stopped container carrying key=value on the bound
// daemon, each with its labels. A query failure is an error, never an empty list.
func (d *Docker) LabeledContainers(ctx context.Context, key, value string) ([]Container, error) {
	if err := downLabel(key, value); err != nil {
		return nil, err
	}
	ids, err := d.boundContainerIDs(ctx, "label="+key+"="+value)
	if err != nil {
		return nil, err
	}
	containers := make([]Container, 0, len(ids))
	for _, id := range ids {
		data, err := d.output(ctx, 64<<10, "inspect", "--format", "{{json .Config.Labels}}", id)
		if err != nil {
			return nil, err
		}
		var labels map[string]string
		if err := json.Unmarshal(bytes.TrimSpace(data), &labels); err != nil {
			return nil, errors.New("decode labels for Docker container " + id)
		}
		containers = append(containers, Container{ID: id, Labels: labels})
	}
	return containers, nil
}

// LabeledNetworkIDs lists network ids carrying key=value on the bound daemon.
func (d *Docker) LabeledNetworkIDs(ctx context.Context, key, value string) ([]string, error) {
	if err := downLabel(key, value); err != nil {
		return nil, err
	}
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}
	data, err := d.output(ctx, 64<<10, "network", "ls", "-q", "--filter", "label="+key+"="+value)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(data))
	for _, id := range ids {
		if !dockerHexID(id) {
			return nil, errors.New("invalid exact Docker network inventory")
		}
	}
	return ids, d.Verify(ctx)
}

// NetworkLabelValue reads one label of a network on the bound daemon.
func (d *Docker) NetworkLabelValue(ctx context.Context, id, key string) (string, error) {
	if !dockerHexID(id) || !dockerToken(key, 128) || strings.ContainsAny(key, `"{}`) {
		return "", errors.New("invalid Docker network label query")
	}
	data, err := d.output(ctx, 4<<10, "network", "inspect", "-f", `{{index .Labels "`+key+`"}}`, id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// LabeledVolumeNames lists the names of volumes carrying key=value on the bound daemon.
func (d *Docker) LabeledVolumeNames(ctx context.Context, key, value string) ([]string, error) {
	if err := downLabel(key, value); err != nil {
		return nil, err
	}
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}
	data, err := d.output(ctx, 64<<10, "volume", "ls", "--filter", "label="+key+"="+value, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !volumeName(name) {
			return nil, errors.New("invalid Docker volume inventory")
		}
		names = append(names, name)
	}
	return names, d.Verify(ctx)
}

// RemoveContainerID force-removes one full container id on the bound daemon and, like
// RemoveContainerContext, treats success as a positively observed exact-id absence. One
// re-observation is enough: docker rm is synchronous, and a container that is genuinely stuck
// stays pending for the caller's persisted todo instead of pinning this command in a retry loop.
func (d *Docker) RemoveContainerID(ctx context.Context, id string) error {
	if !dockerHexID(id) {
		return errors.New("invalid exact Docker container id")
	}
	if err := d.Verify(ctx); err != nil {
		return err
	}
	_, _ = d.output(ctx, 1024, "container", "rm", "--force", id)
	remaining, err := d.boundContainerIDs(ctx, "id="+id)
	if err != nil {
		return err
	}
	if slices.Contains(remaining, id) {
		return errors.New("Docker container remains present; its removal is pending")
	}
	return nil
}

// NetworkInUse reports whether any container, running or stopped, is still attached — the same
// definition the ambient network cleanup uses, queried on the bound daemon.
func (d *Docker) NetworkInUse(ctx context.Context, id string) (bool, error) {
	if !dockerHexID(id) {
		return false, errors.New("invalid exact Docker network id")
	}
	users, err := d.boundContainerIDs(ctx, "network="+id)
	if err != nil {
		return false, err
	}
	return len(users) > 0, nil
}

// RemoveNetworkID removes one network on the bound daemon and proves its absence. The daemon
// itself refuses to remove a network still in use; either outcome leaves callers able to
// distinguish gone from pending.
func (d *Docker) RemoveNetworkID(ctx context.Context, id string) error {
	if !dockerHexID(id) {
		return errors.New("invalid exact Docker network id")
	}
	if err := d.Verify(ctx); err != nil {
		return err
	}
	_, _ = d.output(ctx, 1024, "network", "rm", id)
	data, err := d.output(ctx, 64<<10, "network", "ls", "-q", "--filter", "id="+id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		return errors.New("Docker network remains present; its removal is pending")
	}
	return nil
}
