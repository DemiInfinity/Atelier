package backup

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is an optional S3-compatible destination (AWS, R2, B2, MinIO, …).
// The keys come from the environment and are never shown in the UI.
type S3 struct {
	Endpoint  string // host[:port], or a URL; http:// turns TLS off
	Bucket    string
	Prefix    string
	Region    string
	AccessKey string
	SecretKey string
}

func (s S3) Enabled() bool { return s.Endpoint != "" && s.Bucket != "" }

// Where describes the destination without secrets, e.g. "s3.example.com/bucket/atelier".
func (s S3) Where() string {
	host, _ := s.host()
	w := host + "/" + s.Bucket
	if p := s.prefix(); p != "" {
		w += "/" + p
	}
	return w
}

func (s S3) host() (string, bool) {
	ep := strings.TrimSpace(s.Endpoint)
	if strings.Contains(ep, "://") {
		if u, err := url.Parse(ep); err == nil {
			return u.Host, u.Scheme != "http"
		}
	}
	return strings.TrimRight(ep, "/"), true
}

func (s S3) prefix() string { return strings.Trim(s.Prefix, "/") }

func (s S3) key(name string) string {
	if p := s.prefix(); p != "" {
		return p + "/" + name
	}
	return name
}

func (s S3) client() (*minio.Client, error) {
	if !s.Enabled() {
		return nil, errors.New("S3 is not configured")
	}
	host, secure := s.host()
	return minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(s.AccessKey, s.SecretKey, ""),
		Secure: secure,
		Region: s.Region,
	})
}

// Upload copies a local snapshot to the bucket.
func (s S3) Upload(ctx context.Context, snap Snapshot) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	_, err = c.FPutObject(ctx, s.Bucket, s.key(snap.Name), snap.Path, minio.PutObjectOptions{ContentType: "application/gzip"})
	return err
}

// List returns the snapshots in the bucket, newest first.
func (s S3) List(ctx context.Context) ([]Snapshot, error) {
	c, err := s.client()
	if err != nil {
		return nil, err
	}
	pfx := s.prefix()
	if pfx != "" {
		pfx += "/"
	}
	var out []Snapshot
	for obj := range c.ListObjects(ctx, s.Bucket, minio.ListObjectsOptions{Prefix: pfx}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		name := path.Base(obj.Key)
		if t, ok := ParseName(name); ok && obj.Key == s.key(name) {
			out = append(out, Snapshot{Name: name, Size: obj.Size, Time: t})
		}
	}
	sortSnapshots(out)
	return out, nil
}

// Remove deletes one snapshot from the bucket.
func (s S3) Remove(ctx context.Context, name string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	return c.RemoveObject(ctx, s.Bucket, s.key(name), minio.RemoveObjectOptions{})
}
