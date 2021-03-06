// Copyright 2018 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/gomounts"
)

var kbfusePath = fuse.OSXFUSEPaths{
	DevicePrefix: "/dev/kbfuse",
	Load:         "/Library/Filesystems/kbfuse.fs/Contents/Resources/load_kbfuse",
	Mount:        "/Library/Filesystems/kbfuse.fs/Contents/Resources/mount_kbfuse",
	DaemonVar:    "MOUNT_KBFUSE_DAEMON_PATH",
}

const (
	mountpointTimeout = 5 * time.Second
	notRunningName    = "KBFS_NOT_RUNNING"
)

type symlink struct {
	link string
}

func (s symlink) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	a.Mode = os.ModeSymlink | a.Mode | 0555
	a.Valid = 0
	return nil
}

func (s symlink) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (
	link string, err error) {
	return s.link, nil
}

type cacheEntry struct {
	mountpoint string
	time       time.Time
}

type root struct {
	runmodeStr      string
	runmodeStrFancy string

	lock            sync.RWMutex
	mountpointCache map[uint32]cacheEntry

	getMountsLock sync.Mutex
}

func newRoot() *root {
	runmodeStr := "keybase"
	runmodeStrFancy := "Keybase"
	switch os.Getenv("KEYBASE_RUN_MODE") {
	case "staging":
		runmodeStr = "keybase.staging"
		runmodeStrFancy = "KeybaseStaging"
	case "devel":
		runmodeStr = "keybase.devel"
		runmodeStrFancy = "KeybaseDevel"
	}

	return &root{
		runmodeStr:      runmodeStr,
		runmodeStrFancy: runmodeStrFancy,
		mountpointCache: make(map[uint32]cacheEntry),
	}
}

func (r *root) Root() (fs.Node, error) {
	return r, nil
}

func (r *root) Attr(ctx context.Context, attr *fuse.Attr) error {
	attr.Mode = os.ModeDir | 0555
	return nil
}

func (r *root) getCachedMountpoint(uid uint32) string {
	r.lock.RLock()
	defer r.lock.RUnlock()
	entry, ok := r.mountpointCache[uid]
	if !ok {
		return ""
	}
	now := time.Now()
	if now.Sub(entry.time) > mountpointTimeout {
		// Don't bother deleting the entry, since the caller should
		// just overwrite it.
		return ""
	}
	return entry.mountpoint
}

func (r *root) getMountedVolumes() ([]gomounts.Volume, error) {
	r.getMountsLock.Lock()
	defer r.getMountsLock.Unlock()
	return gomounts.GetMountedVolumes()
}

func (r *root) findKBFSMount(ctx context.Context) (
	mountpoint string, err error) {
	// Get the UID, and crash intentionally if it's not set, because
	// that means we're not compiled against the correct version of
	// bazil.org/fuse.
	uid := ctx.Value(fs.CtxHeaderUIDKey).(uint32)
	// Don't let the root see anything here; we don't want a symlink
	// loop back to this mount.
	if uid == 0 {
		return "", fuse.ENOENT
	}

	mountpoint = r.getCachedMountpoint(uid)
	if mountpoint != "" {
		return mountpoint, nil
	}

	defer func() {
		if err != nil {
			return
		}
		// Cache the entry if we didn't hit an error.
		r.lock.Lock()
		defer r.lock.Unlock()
		r.mountpointCache[uid] = cacheEntry{
			mountpoint: mountpoint,
			time:       time.Now(),
		}
	}()

	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return "", err
	}

	vols, err := r.getMountedVolumes()
	if err != nil {
		return "", err
	}
	fuseType := "fuse"
	if runtime.GOOS == "darwin" {
		fuseType = "kbfuse"
	}
	var fuseMountPoints []string
	for _, v := range vols {
		if v.Type != fuseType {
			continue
		}
		if v.Owner != u.Uid {
			continue
		}
		fuseMountPoints = append(fuseMountPoints, v.Path)
	}

	if len(fuseMountPoints) == 0 {
		return "", fuse.ENOENT
	}

	// Pick the first one alphabetically that has "keybase" in the
	// path.
	sort.Strings(fuseMountPoints)
	for _, mp := range fuseMountPoints {
		// Find mountpoints like "/home/user/.local/share/keybase/fs", or
		// "/home/user/keybase", and make sure it doesn't match mounts for
		// another run mode, say "/home/user/keybase.staging".
		i := strings.Index(mp, r.runmodeStr)
		str := r.runmodeStr
		if i < 0 {
			i = strings.Index(mp, r.runmodeStrFancy)
			str = r.runmodeStrFancy
		}
		if i < 0 {
			continue
		}
		if len(mp) == i+len(str) || mp[i+len(str)] == '/' ||
			mp[i+len(str)] == ' ' {
			return mp, nil
		}
	}

	// Give up.
	return "", fuse.ENOENT
}

func (r *root) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	_, err := r.findKBFSMount(ctx)
	if err != nil {
		if err == fuse.ENOENT {
			// Put a symlink in the directory for someone who's not
			// logged in, so that the directory is non-empty and
			// future redirector calls as root won't try to mount over
			// us.
			return []fuse.Dirent{
				{
					Type: fuse.DT_Link,
					Name: notRunningName,
				},
			}, nil
		}
		return []fuse.Dirent{}, err
	}

	// TODO: show the `kbfs.error.txt" and "kbfs.nologin.txt" files if
	// they exist?  As root, it is hard to figure out if they're
	// there, though.
	return []fuse.Dirent{
		{
			Type: fuse.DT_Link,
			Name: "private",
		},
		{
			Type: fuse.DT_Link,
			Name: "public",
		},
		fuse.Dirent{
			Type: fuse.DT_Link,
			Name: "team",
		},
	}, nil
}

func (r *root) Lookup(
	ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (
	n fs.Node, err error) {
	mountpoint, err := r.findKBFSMount(ctx)
	if err != nil {
		if req.Name == notRunningName {
			return symlink{"/dev/null"}, nil
		}
		return nil, err
	}

	resp.EntryValid = 0
	switch req.Name {
	case "private", "public", "team", ".kbfs_error", ".kbfs_metrics",
		".kbfs_profiles", ".kbfs_reset_caches", ".kbfs_status",
		"kbfs.error.txt", "kbfs.nologin.txt", ".kbfs_enable_auto_journals",
		".kbfs_disable_auto_journals", ".kbfs_enable_block_prefetching",
		".kbfs_disable_block_prefetching", ".kbfs_enable_debug_server",
		".kbfs_disable_debug_server":
		return symlink{filepath.Join(mountpoint, req.Name)}, nil
	}
	return nil, fuse.ENOENT
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <mountpoint>\n", os.Args[0])
		os.Exit(1)
	}

	// Restrict the mountpoint to paths starting with "/keybase".
	// Since this is a suid binary, it is dangerous to allow arbitrary
	// mountpoints.  TODO: Read a redirector mountpoint from a
	// root-owned config file.
	r := newRoot()
	if os.Args[1] != fmt.Sprintf("/%s", r.runmodeStr) {
		fmt.Fprintf(os.Stderr, "ERROR: The redirector may only mount at "+
			"/%s; %s is an invalid mountpoint\n", r.runmodeStr, os.Args[1])
		os.Exit(1)
	}

	currUser, err := user.Current()
	if err != nil {
		panic(err)
	}
	if currUser.Uid != "0" {
		runtime.LockOSThread()
		_, _, errNo := syscall.Syscall(syscall.SYS_SETUID, 0, 0, 0)
		if errNo != 0 {
			fmt.Fprintf(os.Stderr, "Can't setuid: %+v\n", errNo)
			os.Exit(1)
		}
	}

	options := []fuse.MountOption{fuse.AllowOther()}
	options = append(options, fuse.FSName("keybase-redirector"))
	options = append(options, fuse.ReadOnly())
	if runtime.GOOS == "darwin" {
		options = append(options, fuse.OSXFUSELocations(kbfusePath))
		options = append(options, fuse.VolumeName("keybase-redirector"))
		options = append(options, fuse.NoBrowse())
		// Without NoLocalCaches(), OSX will cache symlinks for a long time.
		options = append(options, fuse.NoLocalCaches())
	}

	c, err := fuse.Mount(os.Args[1], options...)
	if err != nil {
		fmt.Printf("Mount error, exiting cleanly: %+v\n", err)
		os.Exit(0)
	}

	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt)
	signal.Notify(interruptChan, syscall.SIGTERM)
	go func() {
		_ = <-interruptChan

		// This might be a different system thread than above, so we
		// might need to setuid again.
		runtime.LockOSThread()
		_, _, errNo := syscall.Syscall(syscall.SYS_SETUID, 0, 0, 0)
		if errNo != 0 {
			fmt.Fprintf(os.Stderr, "Can't setuid: %+v\n", errNo)
			os.Exit(1)
		}
		err := fuse.Unmount(os.Args[1])
		if err != nil {
			fmt.Printf("Couldn't unmount cleanly: %+v", err)
		}
	}()

	srv := fs.New(c, &fs.Config{
		WithContext: func(ctx context.Context, _ fuse.Request) context.Context {
			return context.Background()
		},
	})
	srv.Serve(r)
}
