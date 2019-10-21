// +build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

// ~~~~~~
//
// The Overview of the Remote Snapshotter:
// ----------------------------------------
//
// This is an example implementation of a "remote snapshotter" which can make
// a snapshot which is backed by a remote unpacked layer. The client of this
// snapshotter can get a snapshot WITHOUT downloading the layer contents and let
// this snapshotter lazily downloading necessary data on each access on each
// file in the snapshot (this is known as "lazy pull"), which leads to
// dramatically shorten the container's startup time which includes image
// pulling(when scaling nodes, first deployments, etc...).
//
// This implementation is based on containerd's "overlayfs snapshotter"
// ("github.com/containerd/containerd/snapshots/storage/overlay") and most
// parts are same as the overlay snapshotter.
// The parts which is remote snapshotter specific is labeled by
// "==REMOTE SNAPSHOTTER SPECIFIC==" label so you can relatively easily
// understand the implementation with making sure the differences between
// the remote snapshotter and the normal overlayfs snapshotter.
//
// ~~~~~~

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/pkg/errors"

	fsplugin "github.com/ktock/remote-snapshotter/filesystems"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.SnapshotPlugin,
		ID:   "remote",

		// ==REMOTE SNAPSHOTTER SPECIFIC==
		//
		// We use RemoteFileSystemPlugin to mount unpacked remote layers
		// as remote snapshots.
		// See "/filesystems/plugin.go" in this repo.
		Requires: []plugin.Type{
			fsplugin.RemoteFileSystemPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())
			ic.Meta.Exports["root"] = ic.Root

			// ==REMOTE SNAPSHOTTER SPECIFIC==
			//
			// register all FileSystems which we use to mount unpacked
			// remote layers as remote snapshots.
			var fss []fsplugin.FileSystem
			if ps, err := ic.GetByType(fsplugin.RemoteFileSystemPlugin); err == nil {
				for _, p := range ps {
					if i, err := p.Instance(); err == nil {
						if f, ok := i.(fsplugin.FileSystem); ok {
							fss = append(fss, f)
						}
					}
				}
			}

			return NewSnapshotter(ic.Root, fss, AsynchronousRemove)
		},
	})
}

// SnapshotterConfig is used to configure the remote snapshotter instance
type SnapshotterConfig struct {
	asyncRemove bool
}

// Opt is an option to configure the remote snapshotter
type Opt func(config *SnapshotterConfig) error

// AsynchronousRemove defers removal of filesystem content until
// the Cleanup method is called. Removals will make the snapshot
// referred to by the key unavailable and make the key immediately
// available for re-use.
func AsynchronousRemove(config *SnapshotterConfig) error {
	config.asyncRemove = true
	return nil
}

type snapshotter struct {
	root        string
	ms          *storage.MetaStore
	asyncRemove bool

	// ==REMOTE SNAPSHOTTER SPECIFIC==
	//
	// FileSystems that this snapshotter recognizes.
	fss []fsplugin.FileSystem
}

// NewSnapshotter returns a Snapshotter which can use unpacked remote layers
// as snapshots. This is implemented based on the overlayfs snapshotter, so
// diffs are stored under the provided root and a metadata file is stored under
// the root as same as overlayfs snapshotter.
func NewSnapshotter(root string, fss []fsplugin.FileSystem, opts ...Opt) (snapshots.Snapshotter, error) {
	var config SnapshotterConfig
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	return &snapshotter{
		root:        root,
		ms:          ms,
		asyncRemove: config.asyncRemove,
		fss:         fss,
	}, nil
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer t.Rollback()
	_, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		t.Rollback()
		return snapshots.Info{}, err
	}

	if err := t.Commit(); err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

// Usage returns the resources taken by the snapshot identified by key.
//
// For active snapshots, this will scan the usage of the overlay "diff" (aka
// "upper") directory and may take some time.
//
// For committed snapshots, the value is returned from the metadata database.
func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Usage{}, err
	}
	id, info, usage, err := storage.GetInfo(ctx, key)
	t.Rollback() // transaction no longer needed at this point.

	if err != nil {
		return snapshots.Usage{}, err
	}

	upperPath := o.upperPath(id)

	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			// TODO(stevvooe): Consider not reporting an error in this case.
			return snapshots.Usage{}, err
		}

		usage = snapshots.Usage(du)
	}

	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) (m []mount.Mount, err error) {
	if m, err = o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts); err != nil {
		return
	}

	// ==REMOTE SNAPSHOTTER SPECIFIC==
	//
	// Check if this Prepare() operation is for a remote snapshotter.
	if target, ok := getLabel(snapshots.RemoteSnapshotLabel, opts); ok {

		// ==REMOTE SNAPSHOTTER SPECIFIC==
		//
		// This Prepare() is for a remote snapshotter. Then we commit this snapshot immediately
		// with the label so that the client of this snapshotter can know this is a remote snapshot.
		if err := o.Commit(ctx, target, key, opts...); err != nil {
			return m, nil // fallback to the normal behavior
		}

		// ==REMOTE SNAPSHOTTER SPECIFIC==
		//
		// Return ErrAlreadyExists so that the metadata snapshotter can deal with
		// this remote snapshot correctly.
		return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "target snapshot %q", target)
	}

	return
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := storage.GetSnapshot(ctx, key)
	t.Rollback()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get active mount")
	}
	return o.mounts(s), nil
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	// grab the existing id
	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	usage, err := fs.DiskUsage(ctx, o.upperPath(id))
	if err != nil {
		return err
	}

	// ==REMOTE SNAPSHOTTER SPECIFIC==
	//
	// Check if this Commit() is for a remote snapshot.
	if target, ok := getLabel(snapshots.RemoteSnapshotLabel, opts); ok {
		if target != name {
			err = fmt.Errorf("Invalid label")
			return err
		}
		// This is for a remote snapshot. So we try to mount this layer as a
		// remote snapshot.
		if err = o.mountRemoteSnapshot(o.upperPath(id), opts); err != nil {
			return errors.Wrapf(err, "failed to mount a remote snapshot for %s", target)
		}
		// We successfully made a remote snapshot.
		// Mark it as a remote snapshot so we can distinguish a remote
		// snapshot and normal one when we Stat() it.
		opts = append(opts, snapshots.WithLabels(map[string]string{
			snapshots.RemoteSnapshotLabel: target,
		}))
	}

	if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
		return errors.Wrap(err, "failed to commit snapshot")
	}
	return t.Commit()
}

// Remove abandons the snapshot identified by key. The snapshot will
// immediately become unavailable and unrecoverable. Disk space will
// be freed up on the next call to `Cleanup`.
func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	_, _, err = storage.Remove(ctx, key)
	if err != nil {
		return errors.Wrap(err, "failed to remove")
	}

	if !o.asyncRemove {
		var removals []string
		removals, err = o.getCleanupDirectories(ctx, t)
		if err != nil {
			return errors.Wrap(err, "unable to get directories for removal")
		}

		// Remove directories after the transaction is closed, failures must not
		// return error since the transaction is committed with the removal
		// key no longer available.
		defer func() {
			if err == nil {
				for _, dir := range removals {
					if err := o.cleanupSnapshotDirectory(dir); err != nil {
						log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
					}
				}
			}
		}()

	}

	return t.Commit()
}

// Walk the committed snapshots.
func (o *snapshotter) Walk(ctx context.Context, fn func(context.Context, snapshots.Info) error) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	return storage.WalkInfo(ctx, fn)
}

// Cleanup cleans up disk resources from removed or abandoned snapshots
func (o *snapshotter) Cleanup(ctx context.Context) error {
	cleanup, err := o.cleanupDirectories(ctx)
	if err != nil {
		return err
	}

	for _, dir := range cleanup {
		if err := o.cleanupSnapshotDirectory(dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}

	return nil
}

func (o *snapshotter) cleanupDirectories(ctx context.Context) ([]string, error) {
	// Get a write transaction to ensure no other write transaction can be entered
	// while the cleanup is scanning.
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	defer t.Rollback()
	return o.getCleanupDirectories(ctx, t)
}

func (o *snapshotter) getCleanupDirectories(ctx context.Context, t storage.Transactor) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(o.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}

		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}

	return cleanup, nil
}

func (o *snapshotter) cleanupSnapshotDirectory(dir string) error {

	// ==REMOTE SNAPSHOTTER SPECIFIC==
	//
	// On a remote snapshot, the layer is mounted on the "fs" direcotry.
	// Filesystem can do any finalization by detecting this "unmount" event
	// and we don't care the finalization explicitly at this stage.
	_ = syscall.Unmount(filepath.Join(dir, "fs"), 0)
	if err := os.RemoveAll(dir); err != nil {
		return errors.Wrapf(err, "failed to remove directory %s", dir)
	}
	return nil
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	var td, path string
	defer func() {
		if err != nil {
			if td != "" {
				if err1 := o.cleanupSnapshotDirectory(td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := o.cleanupSnapshotDirectory(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = errors.Wrapf(err, "failed to remove path: %v", err1)
				}
			}
		}
	}()

	snapshotDir := filepath.Join(o.root, "snapshots")
	td, err = o.prepareDirectory(ctx, snapshotDir, kind)
	if err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
		}
		return nil, errors.Wrap(err, "failed to create prepare snapshot dir")
	}
	rollback := true
	defer func() {
		if rollback {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	s, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create snapshot")
	}

	if len(s.ParentIDs) > 0 {
		st, err := os.Stat(o.upperPath(s.ParentIDs[0]))
		if err != nil {
			return nil, errors.Wrap(err, "failed to stat parent")
		}

		stat := st.Sys().(*syscall.Stat_t)

		if err := os.Lchown(filepath.Join(td, "fs"), int(stat.Uid), int(stat.Gid)); err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
			return nil, errors.Wrap(err, "failed to chown")
		}
	}

	path = filepath.Join(snapshotDir, s.ID)
	if err = os.Rename(td, path); err != nil {
		return nil, errors.Wrap(err, "failed to rename")
	}
	td = ""

	rollback = false
	if err = t.Commit(); err != nil {
		return nil, errors.Wrap(err, "commit failed")
	}

	return o.mounts(s), nil
}

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := ioutil.TempDir(snapshotDir, "new-")
	if err != nil {
		return "", errors.Wrap(err, "failed to create temp dir")
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}

	return td, nil
}

func (o *snapshotter) mounts(s storage.Snapshot) []mount.Mount {
	if len(s.ParentIDs) == 0 {
		// if we only have one layer/no parents then just return a bind mount as overlay
		// will not work
		roFlag := "rw"
		if s.Kind == snapshots.KindView {
			roFlag = "ro"
		}

		return []mount.Mount{
			{
				Source: o.upperPath(s.ID),
				Type:   "bind",
				Options: []string{
					roFlag,
					"rbind",
				},
			},
		}
	}
	var options []string

	if s.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", o.workPath(s.ID)),
			fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),
		)
	} else if len(s.ParentIDs) == 1 {
		return []mount.Mount{
			{
				Source: o.upperPath(s.ParentIDs[0]),
				Type:   "bind",
				Options: []string{
					"ro",
					"rbind",
				},
			},
		}
	}

	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i])
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	return []mount.Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}

}

func (o *snapshotter) upperPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

// Close closes the snapshotter
func (o *snapshotter) Close() error {
	return o.ms.Close()
}

// ==REMOTE SNAPSHOTTER SPECIFIC==
//
// mountRemoteSnapshot mounts a remote snapshot on the committed snapshot directory
// as a remote snapshot using one of filesystem this snapshotter recognizes.
func (o *snapshotter) mountRemoteSnapshot(mountpoint string, opts []snapshots.Opt) error {

	// get ref and layer digest from options.
	ref, digest, err := getLayerInfo(opts)
	if err != nil {
		return errors.Wrap(err, "failed to parse basic layer information.")
	}

	// Search a filesystem which can mount a remote snapshot for this layer.
	// TODO: deterministic order.
	for _, f := range o.fss {
		if err := f.Mount(ref, digest, mountpoint); err == nil {
			return nil
		}
	}

	return fmt.Errorf("mountable remote snapshot not found")
}

// ==REMOTE SNAPSHOTTER SPECIFIC==
//
// getLayerInfo is used to extract the layer's basic information which is passed
// through Opt. When the client of this snapshotter want to Prepare() a layer as
// a remote snapshot, these options MUST be passed.
func getLayerInfo(opts []snapshots.Opt) (ref, digest string, err error) {
	var ok bool
	if ref, ok = getLabel(snapshots.RemoteRefLabel, opts); !ok {
		return "", "", errors.New("image reference is not passed")
	}
	if digest, ok = getLabel(snapshots.RemoteDigestLabel, opts); !ok {
		return "", "", errors.New("image digest are not passed")
	}
	return
}

// ==REMOTE SNAPSHOTTER SPECIFIC==
//
// getLabel is a helper function to extract a value which is stored as a value
// of a label in Opts.
func getLabel(label string, opts []snapshots.Opt) (string, bool) {
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return "", false
		}
	}
	if v, ok := base.Labels[label]; ok {
		return v, true
	}
	return "", false
}
