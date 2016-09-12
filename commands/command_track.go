package commands

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/github/git-lfs/config"
	"github.com/github/git-lfs/git"

	"github.com/github/git-lfs/lfs"
	"github.com/spf13/cobra"
)

var (
	prefixBlocklist = []string{
		".git", ".lfs",
	}

	trackVerboseLoggingFlag bool
	trackDryRunFlag         bool
	trackLockableFlag       bool
	trackNotLockableFlag    bool
)

const (
	lockableAttrib = "lockable"
)

func trackCommand(cmd *cobra.Command, args []string) {
	requireGitVersion()

	if config.LocalGitDir == "" {
		Print("Not a git repository.")
		os.Exit(128)
	}

	if config.LocalWorkingDir == "" {
		Print("This operation must be run in a work tree.")
		os.Exit(128)
	}

	lfs.InstallHooks(false)
	knownPaths := findPaths()

	if len(args) == 0 {
		Print("Listing tracked paths")
		for _, t := range knownPaths {
			if t.Lockable {
				Print("    %s [lockable] (%s)", t.Path, t.Source)

			} else {
				Print("    %s (%s)", t.Path, t.Source)

			}
		}
		return
	}

	wd, _ := os.Getwd()
	relpath, err := filepath.Rel(config.LocalWorkingDir, wd)
	if err != nil {
		Exit("Current directory %q outside of git working directory %q.", wd, config.LocalWorkingDir)
	}

	changedAttribLines := make(map[string]string)
ArgsLoop:
	for _, unsanitizedPattern := range args {
		pattern := cleanRootPath(unsanitizedPattern)
		for _, known := range knownPaths {
			if known.Path == filepath.Join(relpath, pattern) && trackLockableFlag == known.Lockable {
				Print("%s already supported", pattern)
				continue ArgsLoop
			}
		}

		// Generate the new / changed attrib line for merging
		encodedArg := strings.Replace(pattern, " ", "[[:space:]]", -1)
		lockableArg := ""
		if trackLockableFlag {
			lockableArg = " " + lockableAttrib
		}

		changedAttribLines[pattern] = fmt.Sprintf("%s filter=lfs diff=lfs merge=lfs -text%v\n", encodedArg, lockableArg)

		Print("Tracking %s", pattern)

	}

	// Now read the whole local attributes file and iterate over the contents,
	// replacing any lines where the values have changed, and appending new lines
	// change this:

	attribContents, err := ioutil.ReadFile(".gitattributes")
	// it's fine for file to not exist
	if err != nil && !os.IsNotExist(err) {
		Print("Error reading .gitattributes file")
		return
	}
	// Re-generate the file with merge of old contents and new (to deal with changes)
	attributesFile, err := os.OpenFile(".gitattributes", os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0660)
	if err != nil {
		Print("Error opening .gitattributes file")
		return
	}
	defer attributesFile.Close()

	if len(attribContents) > 0 {
		scanner := bufio.NewScanner(bytes.NewReader(attribContents))
		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.Fields(line)
			pattern := fields[0]
			if newline, ok := changedAttribLines[pattern]; ok {
				// Replace this line (newline already embedded)
				attributesFile.WriteString(newline)
				// Remove from map so we know we don't have to add it to the end
				delete(changedAttribLines, pattern)
			} else {
				// Write line unchanged (replace newline)
				attributesFile.WriteString(line + "\n")
			}
		}

		// Our method of writing also made sure there's always a newline at end
	}

	// Any items left in the map, write new lines at the end of the file
	for pattern, newline := range changedAttribLines {
		// Newline already embedded
		attributesFile.WriteString(newline)

		// Also, for any new patterns we've added, make sure any existing git
		// tracked files have their timestamp updated so they will now show as
		// modifed note this is relative to current dir which is how we write
		// .gitattributes deliberately not done in parallel as a chan because
		// we'll be marking modified
		//
		// NOTE: `git ls-files` does not do well with leading slashes.
		// Since all `git-lfs track` calls are relative to the root of
		// the repository, the leading slash is simply removed for its
		// implicit counterpart.
		if trackVerboseLoggingFlag {
			Print("Searching for files matching pattern: %s", pattern)
		}
		gittracked, err := git.GetTrackedFiles(pattern)
		if err != nil {
			Exit("Error getting tracked files for %q: %s", pattern, err)
		}

		if trackVerboseLoggingFlag {
			Print("Found %d files previously added to Git matching pattern: %s", len(gittracked), pattern)
		}

		var matchedBlocklist bool
		for _, f := range gittracked {
			if forbidden := blocklistItem(f); forbidden != "" {
				Print("Pattern %s matches forbidden file %s. If you would like to track %s, modify .gitattributes manually.", pattern, f, f)
				matchedBlocklist = true
			}

		}
		if matchedBlocklist {
			continue
		}

		for _, f := range gittracked {
			if trackVerboseLoggingFlag || trackDryRunFlag {
				Print("Git LFS: touching %s", f)
			}

			if !trackDryRunFlag {
				now := time.Now()
				err := os.Chtimes(f, now, now)
				if err != nil {
					LoggedError(err, "Error marking %q modified", f)
					continue
				}
			}
		}
	}
}

type mediaPath struct {
	Path     string
	Source   string
	Lockable bool
}

func findPaths() []mediaPath {
	paths := make([]mediaPath, 0)

	for _, path := range findAttributeFiles() {
		attributes, err := os.Open(path)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(attributes)

		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "filter=lfs") {
				fields := strings.Fields(line)
				relfile, _ := filepath.Rel(config.LocalWorkingDir, path)
				pattern := fields[0]
				if reldir := filepath.Dir(relfile); len(reldir) > 0 {
					pattern = filepath.Join(reldir, pattern)
				}
				lockable := strings.Contains(line, lockableAttrib)
				paths = append(paths, mediaPath{Path: pattern, Source: relfile, Lockable: lockable})
			}
		}
	}

	return paths
}

func findAttributeFiles() []string {
	paths := make([]string, 0)

	repoAttributes := filepath.Join(config.LocalGitDir, "info", "attributes")
	if info, err := os.Stat(repoAttributes); err == nil && !info.IsDir() {
		paths = append(paths, repoAttributes)
	}

	filepath.Walk(config.LocalWorkingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && (filepath.Base(path) == ".gitattributes") {
			paths = append(paths, path)
		}
		return nil
	})

	// reverse the order of the files so more specific entries are found first
	// when iterating from the front (respects precedence)
	for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
		paths[i], paths[j] = paths[j], paths[i]
	}

	return paths
}

// blocklistItem returns the name of the blocklist item preventing the given
// file-name from being tracked, or an empty string, if there is none.
func blocklistItem(name string) string {
	base := filepath.Base(name)

	for _, p := range prefixBlocklist {
		if strings.HasPrefix(base, p) {
			return p
		}
	}

	return ""
}

func init() {
	RegisterCommand("track", trackCommand, func(cmd *cobra.Command) {
		cmd.Flags().BoolVarP(&trackVerboseLoggingFlag, "verbose", "v", false, "log which files are being tracked and modified")
		cmd.Flags().BoolVarP(&trackDryRunFlag, "dry-run", "d", false, "preview results of running `git lfs track`")
		cmd.Flags().BoolVarP(&trackLockableFlag, "lockable", "l", false, "make pattern lockable, i.e. read-only unless locked")
		cmd.Flags().BoolVarP(&trackNotLockableFlag, "not-lockable", "", false, "remove lockable attribute from pattern")
	})
}
