//go:build ignore

// Ent code generation intentionally omits the automatic migration client.
// Runtime schema changes are applied only from the reviewed, versioned SQL in
// internal/meta/migrations and internal/meta/migrations/postgres.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatalf("usage: entgen SCHEMA_PATH TARGET_PATH PACKAGE_PATH")
	}
	defaultStorage, err := gen.NewStorage("sql")
	if err != nil {
		log.Fatal(err)
	}
	storage := *defaultStorage
	storage.SchemaMode &^= gen.Migrate

	cleanupAutomaticMigration := func(next gen.Generator) gen.Generator {
		return gen.GenerateFunc(func(graph *gen.Graph) error {
			if os.Getenv("LO_ENTGEN_TEST_FAIL_AFTER_PREPARE") == "1" {
				return errors.New("forced failure after Ent environment preparation")
			}
			if err := next.Generate(graph); err != nil {
				return err
			}
			for _, directory := range []string{"migrate", "enttest"} {
				path := filepath.Join(graph.Target, directory)
				if err := os.RemoveAll(path); err != nil {
					return fmt.Errorf("remove generated automatic migration directory %q: %w", path, err)
				}
			}
			return nil
		})
	}

	if err := entc.Generate(os.Args[1], &gen.Config{
		Target:  os.Args[2],
		Package: os.Args[3],
		Storage: &storage,
		Hooks:   []gen.Hook{cleanupAutomaticMigration},
	}); err != nil {
		log.Fatalf("running ent codegen: %v", err)
	}
}
