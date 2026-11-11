package backup

// Data directories: file stores beside the database and repositories that a
// snapshot must carry, such as release assets (<data>/releases/) and Git LFS
// objects (<data>/lfs/). Their files go into the archive under the same
// relative paths and are listed in the manifest's Files with sizes and
// checksums, so older Atelier versions still read the archive.
//
// Hidden entries (names starting with ".") are skipped: they hold uploads in
// progress.

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var dataDirs = []string{"releases", "lfs"}

// RegisterDataDir adds a directory (relative to the data dir) to every
// snapshot and full restore. Call it from an init() function.
func RegisterDataDir(name string) {
	for _, d := range dataDirs {
		if d == name {
			return
		}
	}
	dataDirs = append(dataDirs, name)
}

// gitDataDirs are data directories that hold bare repositories laid out as
// <dir>/<shard>/<name>.git (snippets). Snapshots bundle each repository, as
// they do for <data>/repos, instead of copying its files: a plain copy taken
// while a push is landing can hold refs that point at missing objects.
var gitDataDirs []string

// RegisterGitDataDir adds a directory of bare repositories (relative to the
// data dir) to every snapshot and full restore. Call it from an init() function.
func RegisterGitDataDir(name string) {
	RegisterDataDir(name)
	for _, d := range gitDataDirs {
		if d == name {
			return
		}
	}
	gitDataDirs = append(gitDataDirs, name)
}

func isGitDataDir(name string) bool {
	for _, d := range gitDataDirs {
		if d == name {
			return true
		}
	}
	return false
}

// DataDirs lists the directories snapshots include.
func DataDirs() []string { return append([]string(nil), dataDirs...) }

// dataRoot is the data directory: Options.DataDir, or the parent of RepoRoot.
func (o Options) dataRoot() string {
	if o.DataDir != "" {
		return o.DataDir
	}
	return filepath.Dir(o.RepoRoot)
}

// collectDataDirs adds every file in the data directories to the manifest and
// returns where each archive path is read from.
func collectDataDirs(root string, m *Manifest) (map[string]string, error) {
	src := map[string]string{}
	for _, d := range dataDirs {
		if isGitDataDir(d) {
			continue // bundled with the repositories instead
		}
		base := filepath.Join(root, d)
		var files []string
		err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && p == base {
					return filepath.SkipDir
				}
				return err
			}
			if p != base && strings.HasPrefix(e.Name(), ".") {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if e.Type().IsRegular() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		for _, p := range files {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return nil, err
			}
			name := filepath.ToSlash(rel)
			fe, err := fileEntry(p, name)
			if err != nil {
				return nil, err
			}
			m.Files = append(m.Files, fe)
			src[name] = p
		}
	}
	return src, nil
}

// dataDirsIn lists the data directories a manifest has files for, plus every
// registered one (so a full restore never leaves stale files behind).
func dataDirsIn(m Manifest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range dataDirs {
		add(d)
	}
	for _, fe := range m.Files {
		if top, _, ok := strings.Cut(fe.Path, "/"); ok && top != "repos" && path.Clean(top) == top {
			add(top)
		}
	}
	return out
}

// DataDirUsage is the size of one data directory.
func DataDirUsage(dataDir, name string) int64 { return dirSize(filepath.Join(dataDir, name)) }
