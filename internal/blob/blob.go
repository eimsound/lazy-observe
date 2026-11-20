// Package blob provides reusable object storage for large payloads that do not belong in SQLite.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrInvalidKey = errors.New("invalid blob key")
	ErrNotFound   = errors.New("blob not found")
)

type Object struct {
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time,omitempty"`
}

type Store interface {
	Put(context.Context, string, io.Reader) error
	Get(context.Context, string) (io.ReadCloser, error)
	List(context.Context, string) ([]Object, error)
	Delete(context.Context, string) error
	DeletePrefix(context.Context, string) error
}
