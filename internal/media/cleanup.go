package media

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"time"

	"mcp-discord-roleplay/internal/config"
)

// Report says what a cache sweep did. The numbers are what status and the
// startup line print; the reasons are in the log.
type Report struct {
	Removed int   // files deleted
	Bytes   int64 // bytes those files held
	Kept    int   // files still there
	Skipped int   // entries left alone: subdirectories and anything not a regular file
}

// String is the one-line shape the log uses.
func (r Report) String() string {
	return fmt.Sprintf("removed %d file(s), freed %d byte(s), kept %d, skipped %d",
		r.Removed, r.Bytes, r.Kept, r.Skipped)
}

// Cleanup sweeps the media cache and reports what it removed. It is a plain
// function rather than a goroutine or a daemon (user's decision): main runs it
// once at startup, and an operator who wants more can run it from cron —
// "no goroutine" was only ever about not owning one inside the server.
//
// Two rules, in this order:
//
//   - age: anything older than --media-keep goes. --media-keep=0 means the
//     whole cache is disposable at startup, which is a legitimate thing to ask
//     for, and a negative value keeps files forever.
//   - total: if what is left is still over --media-max-mb, the oldest files go
//     until it fits (mtime is the only order we have, and it is the same one
//     the age rule reads). --media-max-mb <= 0 turns this rule off.
//
// The sweep works inside an os.Root on the cache directory and reads it with
// the io/fs helpers built on Root.FS(): one level deep, regular files only. A
// subdirectory, a socket or a dangling symlink that wandered in is skipped and
// logged, never followed and never deleted — we do not remove what we did not
// write.
func Cleanup(cfg *config.Config, log *slog.Logger) (Report, error) {
	var rep Report
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg == nil || !cfg.SaveEnabled() {
		return rep, nil
	}
	dir := cleanDir(cfg.MediaOut)
	if dir == "" {
		return rep, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rep, nil // nothing has ever been saved
		}
		return rep, err
	}
	defer root.Close()
	fsys := root.FS()

	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return rep, err
	}

	type item struct {
		name string
		size int64
		mod  time.Time
	}
	live := make([]item, 0, len(ents))
	now := time.Now()
	expired := cfg.MediaKeep > 0
	cutoff := now.Add(-cfg.MediaKeep)

	for _, e := range ents {
		name := e.Name()
		// fs.Stat follows a symlink, which is what we want to judge: inside a
		// Root a link cannot point outside anyway.
		fi, err := fs.Stat(fsys, name)
		if err != nil {
			rep.Skipped++
			log.Warn("media: cannot stat a cache entry, leaving it", "name", name, "err", err.Error())
			continue
		}
		if !fi.Mode().IsRegular() {
			rep.Skipped++
			log.Warn("media: cache entry is not a regular file, leaving it",
				"name", name, "mode", fi.Mode().String())
			continue
		}
		if expired && fi.ModTime().Before(cutoff) {
			if err := root.Remove(name); err != nil {
				rep.Skipped++
				log.Warn("media: could not delete an expired file", "name", name, "err", err.Error())
				continue
			}
			rep.Removed++
			rep.Bytes += fi.Size()
			continue
		}
		live = append(live, item{name: name, size: fi.Size(), mod: fi.ModTime()})
	}

	if cfg.MediaMaxMB > 0 {
		limit := int64(cfg.MediaMaxMB) << 20
		var total int64
		for _, it := range live {
			total += it.size
		}
		sort.Slice(live, func(i, j int) bool { return live[i].mod.Before(live[j].mod) })
		for i := 0; i < len(live) && total > limit; i++ {
			if err := root.Remove(live[i].name); err != nil {
				rep.Skipped++
				log.Warn("media: could not delete an oversized file", "name", live[i].name, "err", err.Error())
				continue
			}
			total -= live[i].size
			rep.Removed++
			rep.Bytes += live[i].size
			live[i].name = "" // mark it gone
		}
	}

	for _, it := range live {
		if it.name != "" {
			rep.Kept++
		}
	}
	return rep, nil
}
