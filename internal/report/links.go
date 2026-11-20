package report

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// Links lists every existing artifact for one report row, relative to the
// aggregate directory. aggregateLog is a slash-relative file name inside that
// directory; providerDirectory is slash-relative to the cluster directory.
// An empty cluster yields only the aggregate log, and an empty providerDirectory
// stops after cluster-level links. Entries appear in a fixed order and only for
// artifacts that exist when called; files found under logs/ and failed-builds/
// are labeled with their path relative to the aggregate directory.
func Links(aggregateDirectory, aggregateLog, cluster, providerDirectory string) []Link {
	links := make([]Link, 0, 8)
	links = appendFile(links, aggregateDirectory, aggregateLog, "aggregate run.log")
	if cluster == "" {
		return links
	}
	links = appendDir(links, aggregateDirectory, cluster, "cluster directory")
	links = appendFile(links, aggregateDirectory, path.Join(cluster, "run.log"), "cluster run.log")
	if providerDirectory == "" {
		return links
	}
	base := path.Join(cluster, providerDirectory)
	links = appendDir(links, aggregateDirectory, base, "run directory")
	links = appendFile(links, aggregateDirectory, path.Join(base, "run.log"), "run.log")
	links = appendFile(links, aggregateDirectory, path.Join(base, "manifest.json"), "manifest.json")
	links = appendFile(links, aggregateDirectory, path.Join(base, "status.json"), "status.json")
	links = appendFile(links, aggregateDirectory, path.Join(base, "collection-report.json"), "collection-report.json")
	links = appendTree(links, aggregateDirectory, path.Join(base, "logs"))
	links = appendTree(links, aggregateDirectory, path.Join(base, "failed-builds"))
	return links
}

// appendFile adds a link when relative names an existing regular file under
// root. Symlinks are not followed: a symlink to a file is not reported.
func appendFile(links []Link, root, relative, label string) []Link {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || !info.Mode().IsRegular() {
		return links
	}
	return append(links, Link{Label: label, Target: "./" + relative})
}

// appendDir adds a link with a trailing slash when relative names an existing
// directory under root. A symlink to a directory is followed.
func appendDir(links []Link, root, relative, label string) []Link {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || !info.IsDir() {
		return links
	}
	return append(links, Link{Label: label, Target: "./" + relative + "/"})
}

// appendTree adds one link per regular file found under the relative subtree,
// labeling each with its path relative to root and skipping symlinks.
func appendTree(links []Link, root, relative string) []Link {
	base := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		return links
	}
	// Walk errors only mean an artifact vanished mid-walk; skip those files.
	_ = filepath.WalkDir(base, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, current)
		if relErr != nil {
			return nil
		}
		target := filepath.ToSlash(rel)
		links = append(links, Link{Label: target, Target: "./" + target})
		return nil
	})
	return links
}
