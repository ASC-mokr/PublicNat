package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sqlMigration struct {
	Name    string
	Content string
}

// readSQLFiles читает все .sql из директории в алфавитном порядке.
func readSQLFiles(dir string) ([]sqlMigration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]sqlMigration, 0, len(names))
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n, err)
		}
		out = append(out, sqlMigration{Name: n, Content: string(data)})
	}
	return out, nil
}
