package blob

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestStoreContract(t *testing.T) {
	factories := map[string]func(*testing.T) Store{
		"local": func(t *testing.T) Store {
			s, err := NewLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"s3-fake": func(*testing.T) Store { return NewS3WithClient(&memoryS3{objects: map[string][]byte{}}, "test") },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := factory(t)
			if err := store.Put(ctx, "replay/project/session/1.json.gz", strings.NewReader("one")); err != nil {
				t.Fatal(err)
			}
			if err := store.Put(ctx, "replay/project/session/2.json.gz", strings.NewReader("two")); err != nil {
				t.Fatal(err)
			}
			if err := store.Put(ctx, "replay/project/session/1.json.gz", strings.NewReader("replaced")); err != nil {
				t.Fatal(err)
			}
			r, err := store.Get(ctx, "replay/project/session/1.json.gz")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(r)
			r.Close()
			if string(got) != "replaced" {
				t.Fatalf("Get = %q", got)
			}
			objects, err := store.List(ctx, "replay/project/session/")
			if err != nil {
				t.Fatal(err)
			}
			if len(objects) != 2 {
				t.Fatalf("List length = %d", len(objects))
			}
			if err := store.Delete(ctx, "replay/project/session/1.json.gz"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(ctx, "replay/project/session/1.json.gz"); err != ErrNotFound {
				t.Fatalf("Get deleted error = %v", err)
			}
			if err := store.DeletePrefix(ctx, "replay/project/session/"); err != nil {
				t.Fatal(err)
			}
			objects, err = store.List(ctx, "replay/project/session/")
			if err != nil || len(objects) != 0 {
				t.Fatalf("List after DeletePrefix = %v, %v", objects, err)
			}
		})
	}
}

func TestStoresRejectPathTraversal(t *testing.T) {
	stores := map[string]Store{}
	local, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores["local"] = local
	stores["s3-fake"] = NewS3WithClient(&memoryS3{objects: map[string][]byte{}}, "test")
	for name, store := range stores {
		for _, key := range []string{"../escape", "replay/../escape", "/absolute", "replay/a b/x", "replay//x"} {
			t.Run(name+"/"+key, func(t *testing.T) {
				if err := store.Put(context.Background(), key, strings.NewReader("x")); err != ErrInvalidKey {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}

type memoryS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memoryS3) PutObject(
	_ context.Context,
	in *s3.PutObjectInput,
	_ ...func(*s3.Options),
) (*s3.PutObjectOutput, error) {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[aws.ToString(in.Key)] = bytes.Clone(b)
	return &s3.PutObjectOutput{}, nil
}

func (m *memoryS3) GetObject(
	_ context.Context,
	in *s3.GetObjectInput,
	_ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, ErrNotFound
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(bytes.Clone(b)))}, nil
}

func (m *memoryS3) ListObjectsV2(
	_ context.Context,
	in *s3.ListObjectsV2Input,
	_ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0)
	for key := range m.objects {
		if strings.HasPrefix(key, aws.ToString(in.Prefix)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for _, key := range keys {
		size := int64(len(m.objects[key]))
		out.Contents = append(out.Contents, types.Object{Key: aws.String(key), Size: aws.Int64(size)})
	}
	return out, nil
}

func (m *memoryS3) DeleteObject(
	_ context.Context,
	in *s3.DeleteObjectInput,
	_ ...func(*s3.Options),
) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (m *memoryS3) DeleteObjects(
	_ context.Context,
	in *s3.DeleteObjectsInput,
	_ ...func(*s3.Options),
) (*s3.DeleteObjectsOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, obj := range in.Delete.Objects {
		delete(m.objects, aws.ToString(obj.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}
