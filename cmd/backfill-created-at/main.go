// Command backfill-created-at writes a stable created_at into every auth JSON
// under -dir that lacks one, using the file's current mtime (the closest
// locally-available approximation of creation time). Idempotent: files that
// already have created_at are left untouched. Run once per auth dir; back up
// the directory first. After this runs, the loader prefers metadata.created_at
// over the file mtime, so created_at no longer drifts on refresh/restart.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dir := flag.String("dir", "", "auth directory containing *.json files")
	dry := flag.Bool("dry-run", false, "report only, do not write")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: backfill-created-at -dir <auth-dir> [-dry-run]")
		os.Exit(2)
	}

	entries, err := filepath.Glob(filepath.Join(*dir, "*.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "glob: %v\n", err)
		os.Exit(1)
	}

	var stamped, skipped, failed int
	for _, path := range entries {
		info, errStat := os.Stat(path)
		if errStat != nil {
			fmt.Printf("FAIL (stat): %s: %v\n", filepath.Base(path), errStat)
			failed++
			continue
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			fmt.Printf("FAIL (read): %s: %v\n", filepath.Base(path), errRead)
			failed++
			continue
		}
		var m map[string]any
		if errJSON := json.Unmarshal(raw, &m); errJSON != nil {
			fmt.Printf("SKIP (bad json): %s\n", filepath.Base(path))
			failed++
			continue
		}
		if _, ok := m["created_at"]; ok {
			skipped++
			continue
		}

		m["created_at"] = info.ModTime().UTC().Format(time.RFC3339)
		if *dry {
			fmt.Printf("WOULD STAMP %s = %s\n", filepath.Base(path), m["created_at"])
			stamped++
			continue
		}

		out, errMarshal := json.Marshal(m)
		if errMarshal != nil {
			fmt.Printf("FAIL (marshal): %s: %v\n", filepath.Base(path), errMarshal)
			failed++
			continue
		}
		tmp := path + ".tmp"
		if errWrite := os.WriteFile(tmp, out, 0o600); errWrite != nil {
			fmt.Printf("FAIL (write): %s: %v\n", filepath.Base(path), errWrite)
			failed++
			continue
		}
		if errRename := os.Rename(tmp, path); errRename != nil {
			fmt.Printf("FAIL (rename): %s: %v\n", filepath.Base(path), errRename)
			_ = os.Remove(tmp)
			failed++
			continue
		}
		stamped++
	}

	fmt.Printf("done: stamped=%d skipped(existing)=%d failed=%d total=%d\n", stamped, skipped, failed, len(entries))
}
