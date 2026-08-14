// Package tray puts henri's icon in the macOS menu bar while the daemon runs.
//
// A status item needs AppKit, AppKit needs cgo, and henri does not take cgo.
// macOS ships a way around the impasse: osascript's JavaScript-for-Automation
// bridge can drive AppKit from a plain process. So the daemon spawns a small
// supervised osascript child that draws the icon, and the icon disappears when
// the daemon does -- which is what it is for: seeing at a glance that henri is
// running.
//
// Everything else has nothing here on purpose. Linux trays are a protocol
// negotiation with the desktop that no shipped helper performs, and Windows
// needs a message pump henri does not have; on both, `henri status` is the
// answer.
package tray

import (
	"context"
	_ "embed"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

//go:embed tray.js
var script []byte

// icon is the whole README panel, with two changes that keep it visible. Its
// white background is gone: a template image is drawn from the alpha channel
// alone, and an opaque background renders as a solid square. And every path
// carries a thick round-joined stroke on top of its fill, which dilates the
// fine traced line art uniformly -- at eighteen points, hairlines render as
// grey mist next to the solid glyphs a menu bar is full of, and the first
// version of this icon was invisible in exactly that way. The frame is
// thickened to match, so the icon holds a solid footprint even where the art
// inside is sparse.
//
//go:embed icon.svg
var icon []byte

// fastExit separates "the icon cannot be drawn here" from "the icon crashed".
// A child that dies this quickly never drew anything -- an SSH session with no
// window server, most likely -- and retrying will not change the answer.
const fastExit = 5 * time.Second

// giveUpAfter is how many consecutive fast exits henri takes as "not on this
// session" before it stops trying.
const giveUpAfter = 3

// Info is what the menu shows and what its one action runs.
type Info struct {
	// DeviceName labels the menu, so two Macs in one group read differently.
	DeviceName string
	// Binary is the absolute path to henri, for the menu's "send now" action.
	Binary string
}

// Supported reports whether this platform has a menu bar henri can draw in.
func Supported() bool { return runtime.GOOS == "darwin" }

// Start writes the icon and its script where the child can read them and keeps
// the icon drawn until ctx ends. It returns once the child is supervised;
// failures after that are logged, not returned, because a daemon that syncs
// perfectly well should not die over a status item.
func Start(ctx context.Context, log *slog.Logger, info Info) error {
	dir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "henri")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	iconPath := filepath.Join(dir, "tray-icon.svg")
	scriptPath := filepath.Join(dir, "tray.js")
	if err := os.WriteFile(iconPath, icon, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(scriptPath, script, 0o644); err != nil {
		return err
	}
	go supervise(ctx, log, scriptPath, iconPath, info)
	return nil
}

// command builds one invocation of the child. Split out so a test can check
// the argv without a menu bar to draw in.
func command(ctx context.Context, scriptPath, iconPath string, info Info) *exec.Cmd {
	return exec.CommandContext(ctx, "osascript", "-l", "JavaScript", scriptPath,
		strconv.Itoa(os.Getpid()), iconPath, info.DeviceName, info.Binary)
}

// supervise keeps the child alive for as long as the daemon is. The child is
// killed when ctx ends, and takes itself down if the daemon dies uncleanly;
// this loop covers the third case, the child itself crashing.
func supervise(ctx context.Context, log *slog.Logger, scriptPath, iconPath string, info Info) {
	failures := 0
	for ctx.Err() == nil {
		started := time.Now()
		err := command(ctx, scriptPath, iconPath, info).Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) < fastExit {
			failures++
			if failures >= giveUpAfter {
				// No window server to draw in, almost always: a daemon started
				// over SSH, or before login. Sync works fine without the icon,
				// so this is information rather than a fault.
				log.Info("no menu bar to draw henri's icon in; carrying on without it", "err", err)
				return
			}
		} else {
			failures = 0
			log.Debug("menu bar icon exited; drawing it again", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
