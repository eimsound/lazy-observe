package blob

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type S3Config struct {
	Bucket, Region, Endpoint, AccessKey, SecretKey string
	UsePathStyle                                   bool
}

type S3Client interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

type S3 struct {
	client S3Client
	bucket string
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("S3 bucket is required")
	}
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return nil, errors.New("S3 access key and secret key must be set together")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		loadOptions = append(
			loadOptions,
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			),
		)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.UsePathStyle || cfg.Endpoint != ""
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	return NewS3WithClient(client, cfg.Bucket), nil
}

func NewS3WithClient(client S3Client, bucket string) *S3 { return &S3{client: client, bucket: bucket} }

func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	if err := validateKey(key, false); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: r})
	if err != nil {
		return fmt.Errorf("put S3 blob: %w", err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key, false); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if isS3NotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get S3 blob: %w", err)
	}
	return out.Body, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	normalized, err := normalizePrefix(prefix)
	if err != nil {
		return nil, err
	}
	var result []Object
	var token *string
	for {
		out, err := s.client.ListObjectsV2(
			ctx,
			&s3.ListObjectsV2Input{
				Bucket:            aws.String(s.bucket),
				Prefix:            aws.String(normalized),
				ContinuationToken: token,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("list S3 blobs: %w", err)
		}
		for _, obj := range out.Contents {
			item := Object{Key: aws.ToString(obj.Key), Size: aws.ToInt64(obj.Size)}
			if obj.LastModified != nil {
				item.ModTime = obj.LastModified.UTC()
			}
			result = append(result, item)
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	return result, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := validateKey(key, false); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("delete S3 blob: %w", err)
	}
	return nil
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return err
	}
	for start := 0; start < len(objects); start += 1000 {
		end := min(start+1000, len(objects))
		ids := make([]types.ObjectIdentifier, 0, end-start)
		for _, obj := range objects[start:end] {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(obj.Key)})
		}
		out, err := s.client.DeleteObjects(
			ctx,
			&s3.DeleteObjectsInput{
				Bucket: aws.String(s.bucket),
				Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
			},
		)
		if err != nil {
			return fmt.Errorf("delete S3 blob prefix: %w", err)
		}
		if len(out.Errors) != 0 {
			return fmt.Errorf("delete S3 blob prefix: %s", aws.ToString(out.Errors[0].Code))
		}
	}
	return nil
}

func isS3NotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound")
}
