package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// prompt prints the text and returns the string user entered. if empty then defaultValue is returned.
func prompt(text string, defaultValue string) string {
	fmt.Print(text)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			return defaultValue
		}
		return text
	}
	return defaultValue
}

type Data struct {
	Type       string           `json:"type"`
	Timestamps map[string]int64 `json:"timestamps"`
}

const DataFile = "build/incremental.json"

var DataTable Data

func exists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

func saveTable() {
	b, err := json.Marshal(DataTable)
	if err != nil {
		LogF("Incremental", "Marshal", err.Error())
	}
	if err := os.WriteFile(DataFile, b, 0644); err != nil {
		LogF("Incremental", "Write", err.Error())
	}
}

func setCompilationType(Type string) bool {
	if DataTable.Timestamps == nil {
		b, err := os.ReadFile(DataFile)
		if err != nil {
			DataTable.Type = Type
			DataTable.Timestamps = map[string]int64{}
			saveTable()
			return false
		} else if err := json.Unmarshal(b, &DataTable); err != nil {
			LogF("Incremental", "Unmarshal", err.Error())
		}
		if DataTable.Timestamps == nil {
			DataTable.Timestamps = map[string]int64{}
		}
	}
	if DataTable.Type != Type {
		DataTable.Type = Type
		saveTable()
		return true
	}
	return false
}

func getLastRecord(filePath string) int64 {
	if DataTable.Timestamps == nil {
		b, err := os.ReadFile(DataFile)
		if err != nil {
			DataTable.Timestamps = map[string]int64{}
			return 0
		}
		if err := json.Unmarshal(b, &DataTable); err != nil {
			LogF("Incremental", "Unmarshal", err.Error())
		}
		if DataTable.Timestamps == nil {
			DataTable.Timestamps = map[string]int64{}
		}
	}
	return DataTable.Timestamps[filePath]
}

func modTime(filePath string) (int64, bool) {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0, false
	}

	return info.ModTime().UnixNano(), true
}

// hasChanged reports a missing file as unchanged: deletions are handled by
// pruneDeleted, which also removes the artifacts the file produced.
func hasChanged(filePath string) bool {
	actualRecord, ok := modTime(filePath)
	if !ok {
		return false
	}
	return actualRecord != getLastRecord(filePath)
}

func updateRecords(files []string) {
	if DataTable.Timestamps == nil {
		DataTable.Timestamps = map[string]int64{}
	}
	for _, file := range files {
		modTime, ok := modTime(file)
		if !ok {
			continue
		}
		DataTable.Timestamps[file] = modTime
	}
	saveTable()
}

func markAsChanged(files []string) {
	if DataTable.Timestamps == nil {
		DataTable.Timestamps = map[string]int64{}
	}
	for _, file := range files {
		DataTable.Timestamps[file] = 0
	}
}

func rootDir(path string) string {
	if i := strings.IndexByte(path, filepath.Separator); i >= 0 {
		return path[:i]
	}
	return path
}

// pruneDeleted forgets tracked files that are gone and removes the artifacts
// they produced, otherwise a deleted class or resource keeps shipping inside
// the apk. It reports whether linked resources or dexed classes went stale.
func pruneDeleted() (resStale bool, classStale bool) {
	for file := range DataTable.Timestamps {
		if exists(file) {
			continue
		}
		delete(DataTable.Timestamps, file)

		switch rootDir(file) {
		case "res":
			resStale = true
		case "src":
			if removeClasses(file) {
				classStale = true
			}
		case "build":
			// the artifact itself is already gone, the record is enough
			if strings.HasPrefix(file, filepath.Join("build", "flats")+string(filepath.Separator)) {
				resStale = true
			} else {
				classStale = true
			}
		case "jar":
			classStale = true
		case "lib", "assets":
			// these only feed buildBundle, and any stale flag forces it
			resStale = true
		}
	}

	if resStale {
		purgeFlats()
		_, unchanged := getFiles("res", "", true)
		markAsChanged(unchanged)
	}

	return resStale, classStale
}

// purgeFlats empties build/flats. aapt2 names a flat after the resource path
// (res/layout/main.xml becomes layout_main.xml.flat, res/values/styles.xml
// becomes values_styles.arsc.flat), which is not worth guessing here: the whole
// directory is recompiled instead, and aapt2 compile is the cheap stage.
func purgeFlats() {
	flats, err := filepath.Glob(filepath.Join("build", "flats", "*.flat"))
	if err != nil {
		return
	}
	for _, flat := range flats {
		if err := os.Remove(flat); err != nil && !os.IsNotExist(err) {
			LogW("Incremental", "could not remove", flat, err.Error())
		}
	}
}

// removeClasses deletes every compiled unit of a deleted source file, including
// its inner and anonymous classes (Foo.class, Foo$1.class). javac and kotlinc
// both lay classes out by package, so the source path under src/ is matched
// against the class path under build/classes/ instead of the bare file name.
func removeClasses(file string) bool {
	ext := filepath.Ext(file)
	if ext != ".java" && ext != ".kt" {
		return false
	}
	srcDir := filepath.Dir(file)
	name := strings.TrimSuffix(filepath.Base(file), ext)
	rel, err := filepath.Rel(filepath.Join("src"), srcDir)
	if err != nil {
		return false
	}
	target := filepath.Join(filepath.Join("build", "classes"), rel)

	removed := false
	filepath.Walk(filepath.Join("build", "classes"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".class") {
			return nil
		}
		owner := strings.TrimSuffix(filepath.Base(path), ".class")
		if i := strings.IndexByte(owner, '$'); i >= 0 {
			owner = owner[:i]
		}
		if owner == name && filepath.Dir(path) == target {
			if err := os.Remove(path); err == nil {
				removed = true
			} else {
				LogW("Incremental", "could not remove", path, err.Error())
			}
		}
		return nil
	})

	return removed
}

// getFiles returns all the files in the path. if extension is not empty then that is also checked.
func getFiles(path string, extension string, incremental bool) ([]string, []string) {
	changed_files := []string{}
	unchanged_files := []string{}

	filepath.Walk(path, func(path string, f os.FileInfo, err error) error {
		// ignoring traversal errors
		if err != nil {
			return nil
		}
		if f.IsDir() || strings.HasPrefix(f.Name(), ".") {
			return nil
		}
		if extension != "" {
			if strings.HasSuffix(f.Name(), extension) {
				if incremental {
					if hasChanged(path) {
						changed_files = append(changed_files, path)
					} else {
						unchanged_files = append(unchanged_files, path)
					}
				} else {
					changed_files = append(changed_files, path)
				}
			}
		} else {
			if incremental {
				if hasChanged(path) {
					changed_files = append(changed_files, path)
				} else {
					unchanged_files = append(unchanged_files, path)
				}
			} else {
				changed_files = append(changed_files, path)
			}
		}
		return nil
	})

	return changed_files, unchanged_files
}

// copyFiles copies files/directories. ref: https://stackoverflow.com/a/72246196
func copyFiles(src, dst string) error {
	return filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// copy to this path
		outpath := filepath.Join(dst, strings.TrimPrefix(path, src))

		if info.IsDir() {
			os.MkdirAll(outpath, info.Mode())
			return nil // means recursive
		}

		// handle irregular files
		if !info.Mode().IsRegular() {
			switch info.Mode().Type() & os.ModeType {
			case os.ModeSymlink:
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				return os.Symlink(link, outpath)
			}
			return nil
		}

		// copy contents of regular file efficiently

		// open input
		in, _ := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		// create output
		fh, err := os.Create(outpath)
		if err != nil {
			return err
		}
		defer fh.Close()

		// make it the same
		fh.Chmod(info.Mode())

		// copy content
		_, err = io.Copy(fh, in)
		return err
	})
}
