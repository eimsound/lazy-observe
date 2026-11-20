package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Local struct{ root string }

func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	return &Local{root: filepath.Clean(root)}, nil
}

func (s *Local) path(key string, prefix bool) (string, error) {
	if err := validateKey(key, prefix); err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(strings.TrimSuffix(key, "/"))), nil
}

func (s *Local) Put(ctx context.Context, key string, r io.Reader) error {
	path, err := s.path(key, false)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create blob directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return fmt.Errorf("create temporary blob: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = io.Copy(f, &contextReader{ctx: ctx, r: r}); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit blob: %w", err)
	}
	return nil
}

func (s *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := s.path(key, false)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return f, nil
}

func (s *Local) List(ctx context.Context, prefix string) ([]Object, error) {
	normalized, err := normalizePrefix(prefix)
	if err != nil {
		return nil, err
	}
	base, err := s.path(normalized, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(base)
	if errors.Is(err, os.ErrNotExist) {
		return []Object{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list blobs: %w", err)
	}
	if !info.IsDir() {
		return []Object{}, nil
	}
	objects := []Object{}
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".blob-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		objects = append(objects, Object{Key: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime().UTC()})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list blobs: %w", err)
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (s *Local) Delete(_ context.Context, key string) error {
	path, err := s.path(key, false)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

func (s *Local) DeletePrefix(_ context.Context, prefix string) error {
	normalized, err := normalizePrefix(prefix)
	if err != nil {
		return err
	}
	path, err := s.path(normalized, true)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete blob prefix: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("delete blob prefix: %w", err)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
