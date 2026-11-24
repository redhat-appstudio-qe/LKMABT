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
// stops after the cluster run.log. Log files come before the run directory and
// JSON evidence so the report's log list starts where an operator starts:
// run.log. Pod logs stay inside the linked run directory instead of becoming
// hundreds of report rows; curated failed-build logs are linked individually.
// Entries appear in a fixed order and only for artifacts that exist when
// called; files found under failed-builds/ are labeled with their path
// relative to the aggregate directory.
func Links(aggregateDirectory, aggregateLog, cluster, providerDirectory string) []Link {
	if cluster == "" {
		return appendFile(make([]Link, 0, 1), aggregateDirectory, aggregateLog, "aggregate run.log")
	}
	links := make([]Link, 0, 8)
	links = appendFile(links, aggregateDirectory, path.Join(cluster, "run.log"), "run.log")
	if providerDirectory == "" {
		return links
	}
	base := path.Join(cluster, providerDirectory)
	links = appendFile(links, aggregateDirectory, path.Join(base, "run.log"), path.Join(providerDirectory, "run.log"))
	links = appendTree(links, aggregateDirectory, path.Join(base, "failed-builds"), path.Join(providerDirectory, "failed-builds"))
	links = appendDir(links, aggregateDirectory, base, "run directory")
	for _, name := range []string{"manifest.json", "status.json", "collection-report.json"} {
		links = appendFile(links, aggregateDirectory, path.Join(base, name), path.Join(providerDirectory, name))
	}
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
// labeling each with its path below relative prefixed by labelPrefix, so a
// cluster section can omit the repeated cluster directory. Symlinks are skipped.
func appendTree(links []Link, root, relative, labelPrefix string) []Link {
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
		rel, relErr := filepath.Rel(base, current)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		links = append(links, Link{
			Label:  path.Join(labelPrefix, relSlash),
			Target: "./" + path.Join(relative, relSlash),
		})
		return nil
	})
	return links
}
